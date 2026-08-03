package kmsworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db                  *gorm.DB
	seals               *SealKeyring
	now                 func() time.Time
	minimumDeleteDelay  time.Duration
	inventoryMaximumAge time.Duration
	mutationMu          sync.Mutex
	clockMu             sync.Mutex
	lastObservedTime    time.Time
	clockTolerance      time.Duration
}

type ServiceOptions struct {
	Now                 func() time.Time
	MinimumDeleteDelay  time.Duration
	InventoryMaximumAge time.Duration
	ClockTolerance      time.Duration
}

func NewService(db *gorm.DB, seals *SealKeyring, options ServiceOptions) (*Service, error) {
	if db == nil || seals == nil {
		return nil, errors.New("KMS database and sealing keyring are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MinimumDeleteDelay == 0 {
		options.MinimumDeleteDelay = 7 * 24 * time.Hour
	}
	if options.MinimumDeleteDelay < 7*24*time.Hour {
		return nil, errors.New("KMS minimum deletion delay must be at least seven days")
	}
	if options.InventoryMaximumAge == 0 {
		options.InventoryMaximumAge = time.Hour
	}
	if options.InventoryMaximumAge <= 0 {
		return nil, errors.New("KMS inventory maximum age must be positive")
	}
	if options.ClockTolerance == 0 {
		options.ClockTolerance = 2 * time.Second
	}
	if options.ClockTolerance < 0 || options.ClockTolerance > time.Minute {
		return nil, errors.New("KMS clock tolerance is invalid")
	}
	return &Service{
		db: db, seals: seals, now: options.Now,
		minimumDeleteDelay: options.MinimumDeleteDelay, inventoryMaximumAge: options.InventoryMaximumAge,
		clockTolerance: options.ClockTolerance,
	}, nil
}

func (s *Service) Ready(ctx context.Context) error {
	var version KeyVersion
	if err := s.db.WithContext(ctx).Where("state = ?", StateActive).Order("key_id, version").Take(&version).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", errors.New("KMS has no active key version"))
	} else if err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	now, err := s.secureNow()
	if err != nil {
		return err
	}
	if !isEncryptable(version, now) || !isDecryptable(version, now) {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", errors.New("KMS active key is outside its lifecycle window"))
	}
	material, err := s.seals.Open(version.SealKeyID, version.SealedMaterial, managedKeyAAD(version.KeyID, version.Version))
	if err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	defer zeroBytes(material)
	canary := make([]byte, 32)
	if _, err := rand.Read(canary); err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	defer zeroBytes(canary)
	wrapped, err := wrapDataKey(material, version.KeyID, version.Version, canary, []byte("synara-kms-readiness-canary-v1"))
	if err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	defer zeroBytes(wrapped)
	unwrapped, err := unwrapDataKey(material, version.KeyID, version.Version, wrapped, []byte("synara-kms-readiness-canary-v1"))
	if err != nil || !bytes.Equal(unwrapped, canary) {
		zeroBytes(unwrapped)
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", errors.New("KMS readiness canary failed"))
	}
	zeroBytes(unwrapped)
	return nil
}

type MetricsSnapshot struct {
	VersionStates             map[string]int64
	SecondsUntilPrimaryExpiry float64
	PendingDeletions          int64
	SecondsUntilNextDeletion  float64
	SealKeyID                 string
}

func (s *Service) Metrics(ctx context.Context) (MetricsSnapshot, error) {
	type stateCount struct {
		State string
		Count int64
	}
	var rows []stateCount
	if err := s.db.WithContext(ctx).Model(&KeyVersion{}).Select("state, count(*) AS count").Group("state").Scan(&rows).Error; err != nil {
		return MetricsSnapshot{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	result := MetricsSnapshot{
		VersionStates: map[string]int64{
			StateActive: 0, StateDecryptOnly: 0, StateDisabled: 0, StatePendingDeletion: 0, StateDestroyed: 0,
		},
		SecondsUntilPrimaryExpiry: -1, SecondsUntilNextDeletion: -1, SealKeyID: s.seals.PrimaryID(),
	}
	for _, row := range rows {
		result.VersionStates[row.State] = row.Count
	}
	result.PendingDeletions = result.VersionStates[StatePendingDeletion]
	now, err := s.secureNow()
	if err != nil {
		return MetricsSnapshot{}, err
	}
	var activeExpiries []time.Time
	if err := s.db.WithContext(ctx).Model(&KeyVersion{}).Where("state = ? AND encrypt_not_after IS NOT NULL", StateActive).
		Pluck("encrypt_not_after", &activeExpiries).Error; err != nil {
		return MetricsSnapshot{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	for _, expiry := range activeExpiries {
		seconds := expiry.Sub(now).Seconds()
		if result.SecondsUntilPrimaryExpiry < 0 || seconds < result.SecondsUntilPrimaryExpiry {
			result.SecondsUntilPrimaryExpiry = seconds
		}
	}
	var deletionTimes []time.Time
	if err := s.db.WithContext(ctx).Model(&KeyVersion{}).Where("state = ? AND destroy_after IS NOT NULL", StatePendingDeletion).
		Pluck("destroy_after", &deletionTimes).Error; err != nil {
		return MetricsSnapshot{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	for _, deletionTime := range deletionTimes {
		seconds := deletionTime.Sub(now).Seconds()
		if result.SecondsUntilNextDeletion < 0 || seconds < result.SecondsUntilNextDeletion {
			result.SecondsUntilNextDeletion = seconds
		}
	}
	return result, nil
}

func (s *Service) ListAuditEvents(ctx context.Context, actor Actor, limit int) ([]AuditEvent, error) {
	if actor.Role != RoleAuditor {
		return nil, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("auditor role required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var events []AuditEvent
	if err := s.db.WithContext(ctx).Order("occurred_at DESC, id DESC").Limit(limit).Find(&events).Error; err != nil {
		return nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return events, nil
}

func (s *Service) ListResealReceipts(ctx context.Context, actor Actor, limit int) ([]ResealReceipt, error) {
	if actor.Role != RoleAuditor {
		return nil, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("auditor role required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var receipts []ResealReceipt
	if err := s.db.WithContext(ctx).Order("completed_at DESC, id DESC").Limit(limit).Find(&receipts).Error; err != nil {
		return nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return receipts, nil
}

func (s *Service) ListDestructionReceipts(ctx context.Context, actor Actor, limit int) ([]DestructionReceipt, error) {
	if actor.Role != RoleAuditor {
		return nil, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("auditor role required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var receipts []DestructionReceipt
	if err := s.db.WithContext(ctx).Order("destroyed_at DESC, id DESC").Limit(limit).Find(&receipts).Error; err != nil {
		return nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return receipts, nil
}

func (s *Service) CreateKey(ctx context.Context, actor Actor, input CreateKeyInput) (KeyDescription, error) {
	if actor.Role != RoleKeyManager {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key manager role required"))
	}
	now, err := s.secureNow()
	if err != nil {
		return KeyDescription{}, err
	}
	if err := validateCreate(input, now); err != nil {
		return KeyDescription{}, err
	}
	return runIdempotent(s, ctx, actor, "create-key", input.IdempotencyKey, input, http.StatusCreated,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			keyID := uuid.NewString()
			material := make([]byte, 32)
			if _, err := rand.Read(material); err != nil {
				return KeyDescription{}, serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			defer zeroBytes(material)
			sealed, sealID, err := s.seals.Seal(material, managedKeyAAD(keyID, 1))
			if err != nil {
				return KeyDescription{}, serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			labels, _ := json.Marshal(normalizeLabels(input.Labels))
			key := LogicalKey{
				ID: keyID, Name: strings.TrimSpace(input.Name), Description: strings.TrimSpace(input.Description),
				PolicyReference: strings.TrimSpace(input.PolicyReference), LabelsJSON: string(labels),
				MetadataRevision: 1, ActiveVersion: 1, CreatedAt: now, UpdatedAt: now,
			}
			version := KeyVersion{
				KeyID: keyID, Version: 1, Algorithm: AlgorithmAES256GCM, State: StateActive,
				SealKeyID: sealID, SealedMaterial: sealed, EncryptNotAfter: normalizeTime(input.EncryptNotAfter),
				DecryptNotAfter: normalizeTime(input.DecryptNotAfter), CreatedAt: now,
			}
			if err := tx.Create(&key).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if err := tx.Create(&version).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if err := insertAudit(tx, now, actor, "create-key", keyID, int64Pointer(1), "success", "", key); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, []KeyVersion{version}, now), nil
		})
}

func (s *Service) DescribeKey(ctx context.Context, actor Actor, keyID string) (KeyDescription, error) {
	if actor.Role != RoleControlPlaneCryptor && actor.Role != RoleKeyManager && actor.Role != RoleKeyDisabler && actor.Role != RoleDeletionApprover && actor.Role != RoleAuditor {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("describe role required"))
	}
	var key LogicalKey
	if err := s.db.WithContext(ctx).Where("id = ?", keyID).Take(&key).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return KeyDescription{}, serviceError(http.StatusNotFound, "key_not_found", err)
	} else if err != nil {
		return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	var versions []KeyVersion
	if err := s.db.WithContext(ctx).Where("key_id = ?", keyID).Order("version ASC").Find(&versions).Error; err != nil {
		return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	now, err := s.secureNow()
	if err != nil {
		return KeyDescription{}, err
	}
	return describeModels(key, versions, now), nil
}

func (s *Service) UpdateKey(ctx context.Context, actor Actor, keyID string, input UpdateKeyInput) (KeyDescription, error) {
	if actor.Role != RoleKeyManager {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key manager role required"))
	}
	if err := validateUpdate(input); err != nil {
		return KeyDescription{}, err
	}
	return runIdempotent(s, ctx, actor, "update-key:"+keyID, input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			if key.MetadataRevision != input.ExpectedRevision {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("metadata revision changed"))
			}
			if input.Name != nil {
				key.Name = strings.TrimSpace(*input.Name)
			}
			if input.Description != nil {
				key.Description = strings.TrimSpace(*input.Description)
			}
			if input.PolicyReference != nil {
				key.PolicyReference = strings.TrimSpace(*input.PolicyReference)
			}
			if input.Labels != nil {
				labels, _ := json.Marshal(normalizeLabels(*input.Labels))
				key.LabelsJSON = string(labels)
			}
			if input.EncryptNotAfter.Present || input.DecryptNotAfter.Present {
				active := findVersion(versions, key.ActiveVersion)
				if active == nil || active.State != StateActive {
					return KeyDescription{}, serviceError(http.StatusConflict, "key_not_encryptable", errors.New("logical key has no active version"))
				}
				if input.EncryptNotAfter.Present {
					active.EncryptNotAfter = normalizeTime(input.EncryptNotAfter.Value)
				}
				if input.DecryptNotAfter.Present {
					active.DecryptNotAfter = normalizeTime(input.DecryptNotAfter.Value)
				}
				if err := validateExpiry(active.EncryptNotAfter, active.DecryptNotAfter); err != nil {
					return KeyDescription{}, err
				}
				if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ?", keyID, active.Version).
					Updates(map[string]any{"encrypt_not_after": active.EncryptNotAfter, "decrypt_not_after": active.DecryptNotAfter}).Error; err != nil {
					return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
				}
			}
			key.MetadataRevision++
			key.UpdatedAt = now
			result := tx.Model(&LogicalKey{}).Where("id = ? AND metadata_revision = ?", keyID, input.ExpectedRevision).Updates(&key)
			if result.Error != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", result.Error)
			}
			if result.RowsAffected != 1 {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("metadata revision changed"))
			}
			if err := insertAudit(tx, now, actor, "update-key", keyID, nil, "success", "", input); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) RotateKey(ctx context.Context, actor Actor, keyID string, input RotateKeyInput) (KeyDescription, error) {
	if actor.Role != RoleKeyManager {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key manager role required"))
	}
	if !validOperatorReference(input.OperatorReference) {
		return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	if err := validateExpiry(input.EncryptNotAfter, input.DecryptNotAfter); err != nil {
		return KeyDescription{}, err
	}
	return runIdempotent(s, ctx, actor, "rotate-key:"+keyID, input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			var maximum int64
			for index := range versions {
				if versions[index].Version > maximum {
					maximum = versions[index].Version
				}
				if versions[index].State == StatePendingDeletion {
					return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("key has a pending deletion version"))
				}
			}
			if active := findVersion(versions, key.ActiveVersion); active != nil {
				active.State = StateDecryptOnly
				if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ? AND state = ?", keyID, active.Version, StateActive).
					Update("state", StateDecryptOnly).Error; err != nil {
					return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
				}
			}
			newVersion := maximum + 1
			material := make([]byte, 32)
			if _, err := rand.Read(material); err != nil {
				return KeyDescription{}, serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			defer zeroBytes(material)
			sealed, sealID, err := s.seals.Seal(material, managedKeyAAD(keyID, newVersion))
			if err != nil {
				return KeyDescription{}, serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			version := KeyVersion{
				KeyID: keyID, Version: newVersion, Algorithm: AlgorithmAES256GCM, State: StateActive,
				SealKeyID: sealID, SealedMaterial: sealed, EncryptNotAfter: normalizeTime(input.EncryptNotAfter),
				DecryptNotAfter: normalizeTime(input.DecryptNotAfter), CreatedAt: now,
			}
			if err := tx.Create(&version).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			key.ActiveVersion = newVersion
			key.MetadataRevision++
			key.UpdatedAt = now
			if err := tx.Save(&key).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
			versions = append(versions, version)
			if err := insertAudit(tx, now, actor, "rotate-key", keyID, &newVersion, "success", input.OperatorReference, input); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) DisableKey(ctx context.Context, actor Actor, keyID string, version int64, input DisableKeyInput) (KeyDescription, error) {
	if actor.Role != RoleKeyDisabler {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key disabler role required"))
	}
	if !validOperatorReference(input.OperatorReference) {
		return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	return runIdempotent(s, ctx, actor, fmt.Sprintf("disable-key:%s:%d", keyID, version), input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			target := findVersion(versions, version)
			if target == nil {
				return KeyDescription{}, serviceError(http.StatusNotFound, "key_version_not_found", gorm.ErrRecordNotFound)
			}
			if target.State == StateDestroyed || target.State == StatePendingDeletion {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("key version cannot be disabled from its current state"))
			}
			target.State = StateDisabled
			target.DisabledAt = timePointer(now)
			if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ?", keyID, version).
				Updates(map[string]any{"state": StateDisabled, "disabled_at": now}).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
			if key.ActiveVersion == version {
				key.ActiveVersion = 0
				key.MetadataRevision++
				key.UpdatedAt = now
				if err := tx.Save(&key).Error; err != nil {
					return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
				}
			}
			if err := insertAudit(tx, now, actor, "disable-key", keyID, &version, "success", input.OperatorReference, input); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) ScheduleDeletion(ctx context.Context, actor Actor, keyID string, version int64, input ScheduleDeletionInput) (KeyDescription, error) {
	if actor.Role != RoleKeyDisabler {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key disabler role required"))
	}
	if !validOperatorReference(input.OperatorReference) {
		return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	now, err := s.secureNow()
	if err != nil {
		return KeyDescription{}, err
	}
	if err := s.validateInventory(input.Inventory, keyID, version, now); err != nil {
		return KeyDescription{}, err
	}
	return runIdempotent(s, ctx, actor, fmt.Sprintf("schedule-deletion:%s:%d", keyID, version), input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			if input.DestroyAfter.Before(now.Add(s.minimumDeleteDelay)) {
				return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("destruction delay is shorter than the configured minimum"))
			}
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			target := findVersion(versions, version)
			if target == nil {
				return KeyDescription{}, serviceError(http.StatusNotFound, "key_version_not_found", gorm.ErrRecordNotFound)
			}
			if target.State != StateDisabled {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("only a disabled key version can be scheduled for deletion"))
			}
			destroyAfter := input.DestroyAfter.UTC()
			target.State = StatePendingDeletion
			target.PendingDeletionAt = timePointer(now)
			target.DestroyAfter = &destroyAfter
			if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ? AND state = ?", keyID, version, StateDisabled).
				Updates(map[string]any{"state": StatePendingDeletion, "pending_deletion_at": now, "destroy_after": destroyAfter}).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			request := DeletionRequest{
				KeyID: keyID, Version: version, RequestedBy: actor.Identity, OperatorReference: strings.TrimSpace(input.OperatorReference),
				RequestedAt: now, DestroyAfter: destroyAfter, FirstEvidenceID: input.Inventory.EvidenceID,
				FirstEvidenceDigest: strings.ToLower(input.Inventory.Digest), FirstEvidenceAt: input.Inventory.CompletedAt.UTC(),
			}
			encodedEvidence, _ := json.Marshal(input.Inventory)
			request.FirstEvidenceJSON = string(encodedEvidence)
			if err := tx.Create(&request).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if err := insertAudit(tx, now, actor, "schedule-deletion", keyID, &version, "success", input.OperatorReference, input.Inventory); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) CancelDeletion(ctx context.Context, actor Actor, keyID string, version int64, input CancelDeletionInput) (KeyDescription, error) {
	if actor.Role != RoleKeyDisabler {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key disabler role required"))
	}
	if !validOperatorReference(input.OperatorReference) {
		return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	return runIdempotent(s, ctx, actor, fmt.Sprintf("cancel-deletion:%s:%d", keyID, version), input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			target := findVersion(versions, version)
			if target == nil || target.State != StatePendingDeletion {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("key version is not pending deletion"))
			}
			var request DeletionRequest
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key_id = ? AND version = ?", keyID, version).Take(&request).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if request.CancelledAt != nil || request.DestroyedAt != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("deletion request is already terminal"))
			}
			target.State = StateDisabled
			target.PendingDeletionAt = nil
			target.DestroyAfter = nil
			if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ?", keyID, version).
				Updates(map[string]any{"state": StateDisabled, "pending_deletion_at": nil, "destroy_after": nil}).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
			request.CancelledAt = timePointer(now)
			request.CancelledBy = actor.Identity
			if err := tx.Save(&request).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
			if err := insertAudit(tx, now, actor, "cancel-deletion", keyID, &version, "success", input.OperatorReference, nil); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) DestroyKey(ctx context.Context, actor Actor, keyID string, version int64, input DestroyKeyInput) (KeyDescription, error) {
	if actor.Role != RoleDeletionApprover {
		return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("deletion approver role required"))
	}
	if !validOperatorReference(input.OperatorReference) {
		return KeyDescription{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	now, err := s.secureNow()
	if err != nil {
		return KeyDescription{}, err
	}
	if err := s.validateInventory(input.Inventory, keyID, version, now); err != nil {
		return KeyDescription{}, err
	}
	return runIdempotent(s, ctx, actor, fmt.Sprintf("destroy-key:%s:%d", keyID, version), input.IdempotencyKey, input, http.StatusOK,
		func(tx *gorm.DB, now time.Time) (KeyDescription, error) {
			key, versions, err := loadKeyForMutation(tx, keyID)
			if err != nil {
				return KeyDescription{}, err
			}
			target := findVersion(versions, version)
			if target == nil || target.State != StatePendingDeletion || target.DestroyAfter == nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("key version is not pending deletion"))
			}
			if now.Before(*target.DestroyAfter) {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", errors.New("deletion delay has not elapsed"))
			}
			var request DeletionRequest
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key_id = ? AND version = ?", keyID, version).Take(&request).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if request.RequestedBy == actor.Identity {
				return KeyDescription{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("deletion requires an independent approver"))
			}
			if input.Inventory.EvidenceID == request.FirstEvidenceID || !input.Inventory.CompletedAt.After(request.RequestedAt) {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_deletion_inventory_required", errors.New("a second independent inventory is required"))
			}
			target.State = StateDestroyed
			target.SealedMaterial = nil
			target.SealKeyID = ""
			target.DestroyedAt = timePointer(now)
			if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ? AND state = ?", keyID, version, StatePendingDeletion).
				Updates(map[string]any{"state": StateDestroyed, "sealed_material": []byte{}, "seal_key_id": "", "destroyed_at": now}).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			request.DestroyedAt = timePointer(now)
			request.DestroyedBy = actor.Identity
			request.SecondEvidenceID = input.Inventory.EvidenceID
			request.SecondEvidenceDigest = strings.ToLower(input.Inventory.Digest)
			encodedEvidence, _ := json.Marshal(input.Inventory)
			request.SecondEvidenceJSON = string(encodedEvidence)
			if err := tx.Save(&request).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
			receipt := DestructionReceipt{
				ID: uuid.NewString(), KeyID: keyID, Version: version, DestroyedAt: now, DestroyedBy: actor.Identity,
				OperatorReference: strings.TrimSpace(input.OperatorReference), FirstEvidenceID: request.FirstEvidenceID,
				FirstEvidenceDigest: request.FirstEvidenceDigest, SecondEvidenceID: input.Inventory.EvidenceID,
				SecondEvidenceDigest: strings.ToLower(input.Inventory.Digest),
			}
			receipt.ReceiptDigest = digestJSON(receipt)
			if err := tx.Create(&receipt).Error; err != nil {
				return KeyDescription{}, serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			if err := insertAudit(tx, now, actor, "destroy-key", keyID, &version, "success", input.OperatorReference, input.Inventory); err != nil {
				return KeyDescription{}, err
			}
			return describeModels(key, versions, now), nil
		})
}

func (s *Service) Wrap(ctx context.Context, actor Actor, keyID string, version int64, dataKey, aad []byte) (WrapResult, error) {
	if actor.Role != RoleControlPlaneCryptor {
		return WrapResult{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("cryptor role required"))
	}
	if len(dataKey) != 32 || len(aad) == 0 || len(aad) > 64*1024 {
		return WrapResult{}, serviceError(http.StatusBadRequest, "key_authentication_failed", errors.New("data key or AAD is invalid"))
	}
	keyVersion, err := s.loadVersion(ctx, keyID, version)
	if err != nil {
		return WrapResult{}, err
	}
	now, err := s.secureNow()
	if err != nil {
		return WrapResult{}, err
	}
	if !isEncryptable(keyVersion, now) {
		return WrapResult{}, serviceError(http.StatusConflict, "key_not_encryptable", errors.New("key version does not accept Wrap"))
	}
	material, err := s.seals.Open(keyVersion.SealKeyID, keyVersion.SealedMaterial, managedKeyAAD(keyID, version))
	if err != nil {
		return WrapResult{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	defer zeroBytes(material)
	wrapped, err := wrapDataKey(material, keyID, version, dataKey, aad)
	if err != nil {
		return WrapResult{}, serviceError(http.StatusInternalServerError, "key_authentication_failed", err)
	}
	if err := s.insertDataAudit(ctx, now, actor, "wrap", keyID, version, len(wrapped), aad); err != nil {
		zeroBytes(wrapped)
		return WrapResult{}, err
	}
	return WrapResult{KeyID: VersionedKeyID(keyID, version), WrappedDataKey: wrapped}, nil
}

func (s *Service) Unwrap(ctx context.Context, actor Actor, keyID string, version int64, wrapped, aad []byte) ([]byte, error) {
	if actor.Role != RoleControlPlaneCryptor {
		return nil, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("cryptor role required"))
	}
	if len(wrapped) == 0 || len(wrapped) > 4096 || len(aad) == 0 || len(aad) > 64*1024 {
		return nil, serviceError(http.StatusBadRequest, "key_authentication_failed", errors.New("wrapped key or AAD is invalid"))
	}
	keyVersion, err := s.loadVersion(ctx, keyID, version)
	if err != nil {
		return nil, err
	}
	now, err := s.secureNow()
	if err != nil {
		return nil, err
	}
	if !isDecryptable(keyVersion, now) {
		return nil, serviceError(http.StatusConflict, "key_not_decryptable", errors.New("key version does not accept Unwrap"))
	}
	material, err := s.seals.Open(keyVersion.SealKeyID, keyVersion.SealedMaterial, managedKeyAAD(keyID, version))
	if err != nil {
		return nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	defer zeroBytes(material)
	plaintext, err := unwrapDataKey(material, keyID, version, wrapped, aad)
	if err != nil {
		return nil, serviceError(http.StatusBadRequest, "key_authentication_failed", err)
	}
	if err := s.insertDataAudit(ctx, now, actor, "unwrap", keyID, version, len(wrapped), aad); err != nil {
		zeroBytes(plaintext)
		return nil, err
	}
	return plaintext, nil
}

func (s *Service) ResealAll(ctx context.Context, actor Actor, operatorReference string) (ResealReceipt, error) {
	if actor.Role != RoleKeyManager {
		return ResealReceipt{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("key manager role required"))
	}
	if !validOperatorReference(operatorReference) || len(actor.Identity) > 500 {
		return ResealReceipt{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("operator reference is required"))
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	var receipt ResealReceipt
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.secureNow()
		if err != nil {
			return err
		}
		var versions []KeyVersion
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state <> ?", StateDestroyed).Order("key_id, version").Find(&versions).Error; err != nil {
			return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
		}
		oldIDs := map[string]struct{}{}
		var changed int64
		for index := range versions {
			version := &versions[index]
			if version.SealKeyID == s.seals.PrimaryID() {
				continue
			}
			material, err := s.seals.Open(version.SealKeyID, version.SealedMaterial, managedKeyAAD(version.KeyID, version.Version))
			if err != nil {
				return serviceError(http.StatusConflict, "key_authentication_failed", err)
			}
			oldIDs[version.SealKeyID] = struct{}{}
			sealed, sealID, err := s.seals.Seal(material, managedKeyAAD(version.KeyID, version.Version))
			zeroBytes(material)
			if err != nil {
				return serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			if err := tx.Model(&KeyVersion{}).Where("key_id = ? AND version = ? AND seal_key_id = ?", version.KeyID, version.Version, version.SealKeyID).
				Updates(map[string]any{"sealed_material": sealed, "seal_key_id": sealID}).Error; err != nil {
				return serviceError(http.StatusConflict, "key_version_conflict", err)
			}
			changed++
		}
		ids := make([]string, 0, len(oldIDs))
		for id := range oldIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		encodedIDs, _ := json.Marshal(ids)
		completed, err := s.secureNow()
		if err != nil {
			return err
		}
		run := ResealRun{
			ID: uuid.NewString(), StartedAt: started, CompletedAt: completed, ActorIdentity: actor.Identity,
			OperatorReference: strings.TrimSpace(operatorReference), OldSealKeyIDsJSON: string(encodedIDs),
			NewSealKeyID: s.seals.PrimaryID(), VersionCount: changed,
		}
		if err := tx.Create(&run).Error; err != nil {
			return serviceError(http.StatusConflict, "key_version_conflict", err)
		}
		receipt = ResealReceipt{
			ID: uuid.NewString(), RunID: run.ID, StartedAt: started, CompletedAt: completed, ActorIdentity: actor.Identity,
			OperatorReference: strings.TrimSpace(operatorReference), OldSealKeyIDsJSON: string(encodedIDs),
			NewSealKeyID: s.seals.PrimaryID(), VersionCount: changed,
		}
		receipt.ReceiptDigest = digestJSON(receipt)
		if err := tx.Create(&receipt).Error; err != nil {
			return serviceError(http.StatusConflict, "key_version_conflict", err)
		}
		return insertAudit(tx, completed, actor, "reseal", "", nil, "success", operatorReference, receipt)
	})
	return receipt, err
}

func (s *Service) loadVersion(ctx context.Context, keyID string, version int64) (KeyVersion, error) {
	var result KeyVersion
	err := s.db.WithContext(ctx).Where("key_id = ? AND version = ?", keyID, version).Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return KeyVersion{}, serviceError(http.StatusNotFound, "key_version_not_found", err)
	}
	if err != nil {
		return KeyVersion{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return result, nil
}

func (s *Service) insertDataAudit(ctx context.Context, now time.Time, actor Actor, operation, keyID string, version int64, size int, aad []byte) error {
	details := struct {
		Size      int    `json:"size"`
		AADDigest string `json:"aadDigest"`
	}{Size: size, AADDigest: digestBytes(aad)}
	if err := insertAudit(s.db.WithContext(ctx), now, actor, operation, keyID, &version, "success", "", details); err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return nil
}

func (s *Service) validateInventory(evidence InventoryEvidence, keyID string, version int64, now time.Time) error {
	if strings.TrimSpace(evidence.EvidenceID) == "" || len(evidence.EvidenceID) > 200 ||
		strings.TrimSpace(evidence.DeploymentIdentity) == "" || len(evidence.DeploymentIdentity) > 200 ||
		strings.TrimSpace(evidence.DatabaseIdentity) == "" || len(evidence.DatabaseIdentity) > 200 {
		return serviceError(http.StatusConflict, "key_deletion_inventory_required", errors.New("inventory identity is incomplete"))
	}
	if evidence.KeyID != keyID || evidence.Version != version {
		return serviceError(http.StatusConflict, "key_deletion_inventory_required", errors.New("inventory key identity does not match"))
	}
	if evidence.ResourceCount != 0 {
		return serviceError(http.StatusConflict, "key_deletion_inventory_nonzero", errors.New("inventory still references the key version"))
	}
	if _, err := hex.DecodeString(evidence.Digest); err != nil || len(evidence.Digest) != 64 {
		return serviceError(http.StatusConflict, "key_deletion_inventory_required", errors.New("inventory digest is invalid"))
	}
	completed := evidence.CompletedAt.UTC()
	current := now.UTC()
	if completed.IsZero() || completed.After(current.Add(time.Minute)) || current.Sub(completed) > s.inventoryMaximumAge {
		return serviceError(http.StatusConflict, "key_deletion_inventory_required", errors.New("inventory evidence is stale"))
	}
	return nil
}

func runIdempotent[T any](s *Service, ctx context.Context, actor Actor, operation, idempotencyKey string, request any, status int, mutate func(*gorm.DB, time.Time) (T, error)) (T, error) {
	var zero T
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if actor.Identity == "" || len(actor.Identity) > 500 || idempotencyKey == "" || len(idempotencyKey) > 200 {
		return zero, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("actor and Idempotency-Key are required"))
	}
	requestHash := digestJSON(request)
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	var output T
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			lockDigest := sha256.Sum256([]byte(actor.Identity + "\x00" + operation + "\x00" + idempotencyKey))
			lockID := int64(binary.BigEndian.Uint64(lockDigest[:8]))
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockID).Error; err != nil {
				return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
			}
		}
		var existing IdempotencyRecord
		err := tx.Where("actor_identity = ? AND operation = ? AND idempotency_key = ?", actor.Identity, operation, idempotencyKey).Take(&existing).Error
		if err == nil {
			if existing.RequestHash != requestHash {
				return serviceError(http.StatusConflict, "key_version_conflict", errors.New("Idempotency-Key was reused with different input"))
			}
			if err := json.Unmarshal(existing.ResponseJSON, &output); err != nil {
				return serviceError(http.StatusInternalServerError, "kms_unavailable", err)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
		}
		now, err := s.secureNow()
		if err != nil {
			return err
		}
		created, err := mutate(tx, now)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(created)
		if err != nil {
			return serviceError(http.StatusInternalServerError, "kms_unavailable", err)
		}
		record := IdempotencyRecord{
			ActorIdentity: actor.Identity, Operation: operation, IdempotencyKey: idempotencyKey,
			RequestHash: requestHash, StatusCode: status, ResponseJSON: encoded, CreatedAt: now,
		}
		if err := tx.Create(&record).Error; err != nil {
			return serviceError(http.StatusConflict, "key_version_conflict", err)
		}
		output = created
		return nil
	})
	return output, err
}

func (s *Service) secureNow() (time.Time, error) {
	current := s.now().UTC()
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	if !s.lastObservedTime.IsZero() && current.Add(s.clockTolerance).Before(s.lastObservedTime) {
		return time.Time{}, serviceError(http.StatusServiceUnavailable, "kms_unavailable", errors.New("KMS clock moved backwards beyond tolerance"))
	}
	if current.After(s.lastObservedTime) {
		s.lastObservedTime = current
	}
	return current, nil
}

func loadKeyForMutation(tx *gorm.DB, keyID string) (LogicalKey, []KeyVersion, error) {
	var key LogicalKey
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", keyID).Take(&key).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return LogicalKey{}, nil, serviceError(http.StatusNotFound, "key_not_found", err)
	} else if err != nil {
		return LogicalKey{}, nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	var versions []KeyVersion
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key_id = ?", keyID).Order("version ASC").Find(&versions).Error; err != nil {
		return LogicalKey{}, nil, serviceError(http.StatusServiceUnavailable, "kms_unavailable", err)
	}
	return key, versions, nil
}

func insertAudit(tx *gorm.DB, now time.Time, actor Actor, operation, keyID string, version *int64, outcome, operatorReference string, details any) error {
	event := AuditEvent{
		ID: uuid.NewString(), OccurredAt: now.UTC(), ActorIdentity: actor.Identity, ActorRole: string(actor.Role),
		Operation: operation, KeyID: keyID, KeyVersion: version, Outcome: outcome,
		OperatorReference: strings.TrimSpace(operatorReference), DetailsDigest: digestJSON(details),
	}
	if err := tx.Create(&event).Error; err != nil {
		return serviceError(http.StatusServiceUnavailable, "kms_unavailable", fmt.Errorf("write KMS audit event: %w", err))
	}
	return nil
}

func describeModels(key LogicalKey, versions []KeyVersion, now time.Time) KeyDescription {
	labels := map[string]string{}
	_ = json.Unmarshal([]byte(key.LabelsJSON), &labels)
	description := KeyDescription{
		KeyID: key.ID, Name: key.Name, Description: key.Description, PolicyReference: key.PolicyReference,
		Labels: labels, MetadataRevision: key.MetadataRevision, ActiveVersion: key.ActiveVersion,
		CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt, Versions: make([]VersionDescription, 0, len(versions)),
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
	for _, version := range versions {
		description.Versions = append(description.Versions, VersionDescription{
			KeyID: VersionedKeyID(version.KeyID, version.Version), Version: version.Version, Algorithm: version.Algorithm,
			State: version.State, Encryptable: isEncryptable(version, now), Decryptable: isDecryptable(version, now),
			EncryptNotAfter: version.EncryptNotAfter, DecryptNotAfter: version.DecryptNotAfter, CreatedAt: version.CreatedAt,
			DisabledAt: version.DisabledAt, DestroyAfter: version.DestroyAfter, DestroyedAt: version.DestroyedAt,
		})
	}
	return description
}

func isEncryptable(version KeyVersion, now time.Time) bool {
	return version.State == StateActive && (version.EncryptNotAfter == nil || now.Before(*version.EncryptNotAfter))
}

func isDecryptable(version KeyVersion, now time.Time) bool {
	if version.State != StateActive && version.State != StateDecryptOnly {
		return false
	}
	return version.DecryptNotAfter == nil || now.Before(*version.DecryptNotAfter)
}

func findVersion(versions []KeyVersion, version int64) *KeyVersion {
	for index := range versions {
		if versions[index].Version == version {
			return &versions[index]
		}
	}
	return nil
}

func validateCreate(input CreateKeyInput, now time.Time) error {
	if err := validateNameAndLabels(input.Name, input.Description, input.PolicyReference, input.Labels); err != nil {
		return err
	}
	if input.EncryptNotAfter != nil && !input.EncryptNotAfter.After(now) {
		return serviceError(http.StatusBadRequest, "key_not_encryptable", errors.New("encryptNotAfter must be in the future"))
	}
	return validateExpiry(input.EncryptNotAfter, input.DecryptNotAfter)
}

func validateUpdate(input UpdateKeyInput) error {
	if input.ExpectedRevision <= 0 {
		return serviceError(http.StatusBadRequest, "key_version_conflict", errors.New("expectedRevision must be positive"))
	}
	name := "valid"
	description := ""
	policy := ""
	labels := map[string]string{}
	if input.Name != nil {
		name = *input.Name
	}
	if input.Description != nil {
		description = *input.Description
	}
	if input.PolicyReference != nil {
		policy = *input.PolicyReference
	}
	if input.Labels != nil {
		labels = *input.Labels
	}
	return validateNameAndLabels(name, description, policy, labels)
}

func validateNameAndLabels(name, description, policy string, labels map[string]string) error {
	if value := strings.TrimSpace(name); value == "" || len(value) > 200 || strings.ContainsAny(value, "\r\n\t") {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("key name is invalid"))
	}
	if len(description) > 2000 || len(policy) > 500 || strings.ContainsAny(policy, "\r\n\t") {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("key metadata is invalid"))
	}
	if len(labels) > 32 {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("too many labels"))
	}
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || len(key) > 100 || len(value) > 500 || strings.ContainsAny(key+value, "\r\n\t") {
			return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("key label is invalid"))
		}
	}
	return nil
}

func validOperatorReference(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 500 && !strings.ContainsAny(value, "\r\n\t")
}

func validateExpiry(encrypt, decrypt *time.Time) error {
	if encrypt != nil && decrypt != nil && !decrypt.After(*encrypt) {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("decryptNotAfter must be later than encryptNotAfter"))
	}
	return nil
}

func normalizeLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return result
}

func normalizeTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return digestBytes(encoded)
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func timePointer(value time.Time) *time.Time { normalized := value.UTC(); return &normalized }
func int64Pointer(value int64) *int64        { return &value }
