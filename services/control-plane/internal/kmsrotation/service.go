package kmsrotation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const (
	ResourceProviderCredential   = "provider_credential"
	ResourceIdentityConnection   = "identity_connection"
	ResourceIdentityLoginAttempt = "identity_login_attempt"
	defaultBatchSize             = 200
	maxBatchSize                 = 1000
)

var ErrConcurrentMutation = errors.New("encrypted resource changed during KMS rewrap")

type Service struct {
	db     *gorm.DB
	cipher *credentialkms.EnvelopeCipher
	now    func() time.Time
}

type InventoryEntry struct {
	ResourceType  string `json:"resourceType"`
	KMSProvider   string `json:"kmsProvider"`
	KMSKeyID      string `json:"kmsKeyId"`
	Count         int64  `json:"count"`
	EnvelopeValid bool   `json:"envelopeValid"`
	Decryptable   bool   `json:"decryptable"`
	Primary       bool   `json:"primary"`
}

type Plan struct {
	PrimaryProvider string           `json:"primaryProvider"`
	PrimaryKeyID    string           `json:"primaryKeyId"`
	Inventory       []InventoryEntry `json:"inventory"`
	AlreadyPrimary  int64            `json:"alreadyPrimary"`
	PendingRewrap   int64            `json:"pendingRewrap"`
	Unconfigured    int64            `json:"unconfigured"`
}

type ExecuteOptions struct {
	OperatorReference string
	BatchSize         int
	ResumeRunID       *uuid.UUID
}

type Report struct {
	RunID                     uuid.UUID `json:"runId"`
	PrimaryProvider           string    `json:"primaryProvider"`
	PrimaryKeyID              string    `json:"primaryKeyId"`
	ProviderCredentialCount   int64     `json:"providerCredentialCount"`
	IdentityConnectionCount   int64     `json:"identityConnectionCount"`
	IdentityLoginAttemptCount int64     `json:"identityLoginAttemptCount"`
	EntryDigest               string    `json:"entryDigest"`
	CompletedAt               time.Time `json:"completedAt"`
}

func New(db *gorm.DB, cipher *credentialkms.EnvelopeCipher) (*Service, error) {
	if db == nil {
		return nil, errors.New("KMS rotation database is required")
	}
	provider, keyID := cipher.PrimaryIdentity()
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(keyID) == "" {
		return nil, errors.New("KMS rotation requires a configured primary key")
	}
	return &Service{db: db, cipher: cipher, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Plan(ctx context.Context) (Plan, error) {
	provider, keyID := s.cipher.PrimaryIdentity()
	result := Plan{PrimaryProvider: provider, PrimaryKeyID: keyID}
	for _, resource := range encryptedResources {
		entries, err := resource.inventory(ctx, s.db)
		if err != nil {
			return Plan{}, err
		}
		for _, entry := range entries {
			entry.Primary = entry.EnvelopeValid && entry.KMSProvider == provider && entry.KMSKeyID == keyID
			entry.Decryptable = entry.EnvelopeValid && s.cipher.CanDecrypt(entry.KMSProvider, entry.KMSKeyID)
			if entry.Primary {
				result.AlreadyPrimary += entry.Count
			} else {
				result.PendingRewrap += entry.Count
				if !entry.Decryptable {
					result.Unconfigured += entry.Count
				}
			}
			result.Inventory = append(result.Inventory, entry)
		}
	}
	sort.Slice(result.Inventory, func(left, right int) bool {
		if result.Inventory[left].ResourceType != result.Inventory[right].ResourceType {
			return result.Inventory[left].ResourceType < result.Inventory[right].ResourceType
		}
		if result.Inventory[left].KMSProvider != result.Inventory[right].KMSProvider {
			return result.Inventory[left].KMSProvider < result.Inventory[right].KMSProvider
		}
		return result.Inventory[left].KMSKeyID < result.Inventory[right].KMSKeyID
	})
	return result, nil
}

func (s *Service) Execute(ctx context.Context, options ExecuteOptions) (Report, error) {
	operatorReference := strings.TrimSpace(options.OperatorReference)
	if len(operatorReference) < 3 || len(operatorReference) > 200 {
		return Report{}, errors.New("KMS rotation operator reference must contain 3 to 200 characters")
	}
	batchSize := options.BatchSize
	if batchSize == 0 {
		batchSize = defaultBatchSize
	}
	if batchSize < 1 || batchSize > maxBatchSize {
		return Report{}, fmt.Errorf("KMS rotation batch size must be between 1 and %d", maxBatchSize)
	}
	plan, err := s.Plan(ctx)
	if err != nil {
		return Report{}, err
	}
	if plan.Unconfigured > 0 {
		return Report{}, fmt.Errorf("KMS rotation cannot decrypt %d encrypted resources; configure every old key as decrypt-only first", plan.Unconfigured)
	}
	run, err := s.resolveRun(ctx, operatorReference, options.ResumeRunID)
	if err != nil {
		return Report{}, err
	}

	for _, resource := range encryptedResources {
		if err := s.rewrapResource(ctx, run, resource, batchSize); err != nil {
			return Report{}, err
		}
	}
	remaining, err := s.Plan(ctx)
	if err != nil {
		return Report{}, err
	}
	if remaining.PendingRewrap != 0 {
		return Report{}, fmt.Errorf("KMS rotation did not converge: %d encrypted resources remain on non-primary keys", remaining.PendingRewrap)
	}
	return s.complete(ctx, run)
}

func (s *Service) resolveRun(
	ctx context.Context,
	operatorReference string,
	resumeRunID *uuid.UUID,
) (persistence.KMSRewrapRun, error) {
	primaryProvider, primaryKeyID := s.cipher.PrimaryIdentity()
	if resumeRunID == nil {
		run := persistence.KMSRewrapRun{
			ID: uuid.New(), PrimaryProvider: primaryProvider, PrimaryKeyID: primaryKeyID,
			OperatorReference: operatorReference, StartedAt: s.now(),
		}
		if err := s.db.WithContext(ctx).Create(&run).Error; err != nil {
			return persistence.KMSRewrapRun{}, fmt.Errorf("create KMS rewrap run: %w", err)
		}
		return run, nil
	}
	var run persistence.KMSRewrapRun
	if err := s.db.WithContext(ctx).First(&run, "id = ?", *resumeRunID).Error; err != nil {
		return persistence.KMSRewrapRun{}, fmt.Errorf("load KMS rewrap run: %w", err)
	}
	if run.PrimaryProvider != primaryProvider || run.PrimaryKeyID != primaryKeyID {
		return persistence.KMSRewrapRun{}, errors.New("KMS rewrap run targets a different primary key")
	}
	if run.OperatorReference != operatorReference {
		return persistence.KMSRewrapRun{}, errors.New("KMS rewrap run operator reference does not match")
	}
	var receiptCount int64
	if err := s.db.WithContext(ctx).Model(&persistence.KMSRewrapReceipt{}).
		Where("run_id = ?", run.ID).Count(&receiptCount).Error; err != nil {
		return persistence.KMSRewrapRun{}, fmt.Errorf("inspect KMS rewrap receipt: %w", err)
	}
	if receiptCount != 0 {
		return persistence.KMSRewrapRun{}, errors.New("KMS rewrap run is already complete")
	}
	return run, nil
}

type encryptedResource struct {
	resourceType string
	inventory    func(context.Context, *gorm.DB) ([]InventoryEntry, error)
	loadBatch    func(context.Context, *gorm.DB, string, string, int) ([]rewrapCandidate, error)
	update       func(context.Context, *gorm.DB, rewrapCandidate, credentialkms.Envelope) (int64, error)
}

type rewrapCandidate struct {
	tenantID uuid.UUID
	id       uuid.UUID
	aad      []byte
	envelope credentialkms.Envelope
}

func (s *Service) rewrapResource(
	ctx context.Context,
	run persistence.KMSRewrapRun,
	resource encryptedResource,
	batchSize int,
) error {
	primaryProvider, primaryKeyID := s.cipher.PrimaryIdentity()
	for {
		candidates, err := resource.loadBatch(ctx, s.db, primaryProvider, primaryKeyID, batchSize)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		progress := false
		for _, candidate := range candidates {
			next, changed, err := s.cipher.RewrapToPrimary(ctx, candidate.envelope, candidate.aad)
			if err != nil {
				return fmt.Errorf("rewrap %s %s: %w", resource.resourceType, candidate.id, err)
			}
			if !changed {
				continue
			}
			err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				entry := persistence.KMSRewrapEntry{
					ID: uuid.New(), RunID: run.ID, TenantID: candidate.tenantID,
					ResourceType: resource.resourceType, ResourceID: candidate.id,
					OldKMSProvider: candidate.envelope.KMSProvider, OldKMSKeyID: candidate.envelope.KMSKeyID,
					OldEncryptedDataKey: candidate.envelope.EncryptedDataKey,
					NewKMSProvider:      next.KMSProvider, NewKMSKeyID: next.KMSKeyID,
					NewEncryptedDataKey: next.EncryptedDataKey, CreatedAt: s.now(),
				}
				if err := tx.Create(&entry).Error; err != nil {
					return fmt.Errorf("record KMS rewrap authorization: %w", err)
				}
				rows, err := resource.update(ctx, tx, candidate, next)
				if err != nil {
					return err
				}
				if rows != 1 {
					return ErrConcurrentMutation
				}
				resourceID := candidate.id
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: candidate.tenantID, ActorType: "system",
					Action: "security.credential_kms_rewrapped", ResourceType: resource.resourceType,
					ResourceID: &resourceID, RequestID: run.OperatorReference,
					Metadata: map[string]any{
						"runId": run.ID, "fromKmsProvider": candidate.envelope.KMSProvider,
						"fromKmsKeyId":  candidate.envelope.KMSKeyID,
						"toKmsProvider": next.KMSProvider, "toKmsKeyId": next.KMSKeyID,
					},
				})
			})
			if errors.Is(err, ErrConcurrentMutation) {
				continue
			}
			if err != nil {
				return fmt.Errorf("persist KMS rewrap for %s %s: %w", resource.resourceType, candidate.id, err)
			}
			progress = true
		}
		if !progress {
			remaining, err := resource.loadBatch(ctx, s.db, primaryProvider, primaryKeyID, 1)
			if err != nil {
				return err
			}
			if len(remaining) == 0 {
				return nil
			}
			return ErrConcurrentMutation
		}
	}
}

func (s *Service) complete(ctx context.Context, run persistence.KMSRewrapRun) (Report, error) {
	var entries []persistence.KMSRewrapEntry
	if err := s.db.WithContext(ctx).
		Where("run_id = ?", run.ID).
		Order("resource_type ASC, resource_id ASC, id ASC").Find(&entries).Error; err != nil {
		return Report{}, fmt.Errorf("load KMS rewrap evidence: %w", err)
	}
	hash := sha256.New()
	counts := map[string]int64{}
	for _, entry := range entries {
		counts[entry.ResourceType]++
		for _, value := range [][]byte{
			entry.ID[:], entry.TenantID[:], []byte(entry.ResourceType), entry.ResourceID[:],
			[]byte(entry.OldKMSProvider), []byte(entry.OldKMSKeyID), entry.OldEncryptedDataKey,
			[]byte(entry.NewKMSProvider), []byte(entry.NewKMSKeyID), entry.NewEncryptedDataKey,
		} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write(value)
		}
	}
	digest := hash.Sum(nil)
	receipt := persistence.KMSRewrapReceipt{
		RunID:                     run.ID,
		ProviderCredentialCount:   counts[ResourceProviderCredential],
		IdentityConnectionCount:   counts[ResourceIdentityConnection],
		IdentityLoginAttemptCount: counts[ResourceIdentityLoginAttempt],
		EntryDigest:               digest, CompletedAt: s.now(),
	}
	if err := s.db.WithContext(ctx).Create(&receipt).Error; err != nil {
		return Report{}, fmt.Errorf("record KMS rewrap receipt: %w", err)
	}
	return Report{
		RunID: run.ID, PrimaryProvider: run.PrimaryProvider, PrimaryKeyID: run.PrimaryKeyID,
		ProviderCredentialCount:   receipt.ProviderCredentialCount,
		IdentityConnectionCount:   receipt.IdentityConnectionCount,
		IdentityLoginAttemptCount: receipt.IdentityLoginAttemptCount,
		EntryDigest:               fmt.Sprintf("%x", digest), CompletedAt: receipt.CompletedAt,
	}, nil
}

type inventoryRow struct {
	KMSProvider   string
	KMSKeyID      string
	EnvelopeValid bool
	Count         int64
}

func inventoryQuery(
	ctx context.Context,
	db *gorm.DB,
	resourceType string,
	table string,
	payloadColumn string,
) ([]InventoryEntry, error) {
	var rows []inventoryRow
	validExpression := "CASE WHEN length(" + payloadColumn + ") > 0 AND encrypted_data_key IS NOT NULL " +
		"AND length(encrypted_data_key) > 0 AND kms_provider IS NOT NULL AND length(trim(kms_provider)) > 0 " +
		"AND kms_key_id IS NOT NULL AND length(trim(kms_key_id)) > 0 THEN true ELSE false END"
	err := db.WithContext(ctx).Table(table).
		Select("COALESCE(kms_provider, '') AS kms_provider, COALESCE(kms_key_id, '') AS kms_key_id, " + validExpression + " AS envelope_valid, COUNT(*) AS count").
		Where(payloadColumn + " IS NOT NULL").
		Group("COALESCE(kms_provider, ''), COALESCE(kms_key_id, ''), " + validExpression).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("inventory %s KMS envelopes: %w", resourceType, err)
	}
	entries := make([]InventoryEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, InventoryEntry{
			ResourceType: resourceType, KMSProvider: row.KMSProvider,
			KMSKeyID: row.KMSKeyID, Count: row.Count, EnvelopeValid: row.EnvelopeValid,
		})
	}
	return entries, nil
}

var encryptedResources = []encryptedResource{
	{
		resourceType: ResourceProviderCredential,
		inventory: func(ctx context.Context, db *gorm.DB) ([]InventoryEntry, error) {
			return inventoryQuery(ctx, db, ResourceProviderCredential, "provider_credentials", "encrypted_payload")
		},
		loadBatch: loadCredentialBatch,
		update: func(ctx context.Context, tx *gorm.DB, candidate rewrapCandidate, next credentialkms.Envelope) (int64, error) {
			result := tx.WithContext(ctx).Model(&persistence.ProviderCredential{}).
				Where("id = ? AND tenant_id = ? AND encrypted_payload = ? AND encrypted_data_key = ? AND kms_provider = ? AND kms_key_id = ?",
					candidate.id, candidate.tenantID, candidate.envelope.EncryptedPayload,
					candidate.envelope.EncryptedDataKey, candidate.envelope.KMSProvider, candidate.envelope.KMSKeyID).
				UpdateColumns(map[string]any{
					"encrypted_data_key": next.EncryptedDataKey,
					"kms_provider":       next.KMSProvider, "kms_key_id": next.KMSKeyID,
				})
			return result.RowsAffected, result.Error
		},
	},
	{
		resourceType: ResourceIdentityConnection,
		inventory: func(ctx context.Context, db *gorm.DB) ([]InventoryEntry, error) {
			return inventoryQuery(ctx, db, ResourceIdentityConnection, "identity_connections", "encrypted_secret")
		},
		loadBatch: loadConnectionBatch,
		update: func(ctx context.Context, tx *gorm.DB, candidate rewrapCandidate, next credentialkms.Envelope) (int64, error) {
			result := tx.WithContext(ctx).Model(&persistence.IdentityConnection{}).
				Where("id = ? AND tenant_id = ? AND encrypted_secret = ? AND encrypted_data_key = ? AND kms_provider = ? AND kms_key_id = ?",
					candidate.id, candidate.tenantID, candidate.envelope.EncryptedPayload,
					candidate.envelope.EncryptedDataKey, candidate.envelope.KMSProvider, candidate.envelope.KMSKeyID).
				UpdateColumns(map[string]any{
					"encrypted_data_key": next.EncryptedDataKey,
					"kms_provider":       next.KMSProvider, "kms_key_id": next.KMSKeyID,
				})
			return result.RowsAffected, result.Error
		},
	},
	{
		resourceType: ResourceIdentityLoginAttempt,
		inventory: func(ctx context.Context, db *gorm.DB) ([]InventoryEntry, error) {
			return inventoryQuery(ctx, db, ResourceIdentityLoginAttempt, "identity_login_attempts", "encrypted_payload")
		},
		loadBatch: loadLoginAttemptBatch,
		update: func(ctx context.Context, tx *gorm.DB, candidate rewrapCandidate, next credentialkms.Envelope) (int64, error) {
			result := tx.WithContext(ctx).Model(&persistence.IdentityLoginAttempt{}).
				Where("id = ? AND tenant_id = ? AND encrypted_payload = ? AND encrypted_data_key = ? AND kms_provider = ? AND kms_key_id = ?",
					candidate.id, candidate.tenantID, candidate.envelope.EncryptedPayload,
					candidate.envelope.EncryptedDataKey, candidate.envelope.KMSProvider, candidate.envelope.KMSKeyID).
				UpdateColumns(map[string]any{
					"encrypted_data_key": next.EncryptedDataKey,
					"kms_provider":       next.KMSProvider, "kms_key_id": next.KMSKeyID,
				})
			return result.RowsAffected, result.Error
		},
	},
}

func loadCredentialBatch(
	ctx context.Context,
	db *gorm.DB,
	primaryProvider string,
	primaryKeyID string,
	limit int,
) ([]rewrapCandidate, error) {
	var models []persistence.ProviderCredential
	if err := nonPrimaryQuery(db.WithContext(ctx), primaryProvider, primaryKeyID).
		Order("id ASC").Limit(limit).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("load Provider Credential KMS rewrap batch: %w", err)
	}
	result := make([]rewrapCandidate, 0, len(models))
	for _, model := range models {
		aad, err := credentials.EnvelopeAAD(model)
		if err != nil {
			return nil, fmt.Errorf("build Provider Credential AAD for %s: %w", model.ID, err)
		}
		result = append(result, rewrapCandidate{
			tenantID: model.TenantID, id: model.ID, aad: aad,
			envelope: credentialkms.Envelope{
				EncryptedPayload: model.EncryptedPayload, EncryptedDataKey: model.EncryptedDataKey,
				KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID,
			},
		})
	}
	return result, nil
}

func loadConnectionBatch(
	ctx context.Context,
	db *gorm.DB,
	primaryProvider string,
	primaryKeyID string,
	limit int,
) ([]rewrapCandidate, error) {
	var models []persistence.IdentityConnection
	if err := nonPrimaryQuery(db.WithContext(ctx), primaryProvider, primaryKeyID).
		Where("encrypted_secret IS NOT NULL AND length(encrypted_secret) > 0").
		Order("id ASC").Limit(limit).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("load identity Connection KMS rewrap batch: %w", err)
	}
	result := make([]rewrapCandidate, 0, len(models))
	for _, model := range models {
		if model.KMSProvider == nil || model.KMSKeyID == nil {
			return nil, fmt.Errorf("identity Connection %s has encrypted data without KMS identity", model.ID)
		}
		result = append(result, rewrapCandidate{
			tenantID: model.TenantID, id: model.ID,
			aad: enterpriseidentity.ConnectionEnvelopeAAD(model.TenantID, model.ID, model.Kind),
			envelope: credentialkms.Envelope{
				EncryptedPayload: model.EncryptedSecret, EncryptedDataKey: model.EncryptedDataKey,
				KMSProvider: *model.KMSProvider, KMSKeyID: *model.KMSKeyID,
			},
		})
	}
	return result, nil
}

func loadLoginAttemptBatch(
	ctx context.Context,
	db *gorm.DB,
	primaryProvider string,
	primaryKeyID string,
	limit int,
) ([]rewrapCandidate, error) {
	var models []persistence.IdentityLoginAttempt
	if err := nonPrimaryQuery(db.WithContext(ctx), primaryProvider, primaryKeyID).
		Where("encrypted_payload IS NOT NULL AND length(encrypted_payload) > 0").
		Order("id ASC").Limit(limit).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("load identity Login Attempt KMS rewrap batch: %w", err)
	}
	result := make([]rewrapCandidate, 0, len(models))
	for _, model := range models {
		result = append(result, rewrapCandidate{
			tenantID: model.TenantID, id: model.ID,
			aad: enterpriseidentity.LoginAttemptEnvelopeAAD(model.TenantID, model.ID, model.ConnectionID),
			envelope: credentialkms.Envelope{
				EncryptedPayload: model.EncryptedPayload, EncryptedDataKey: model.EncryptedDataKey,
				KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID,
			},
		})
	}
	return result, nil
}

func nonPrimaryQuery(db *gorm.DB, primaryProvider, primaryKeyID string) *gorm.DB {
	return db.Where("encrypted_data_key IS NOT NULL AND length(encrypted_data_key) > 0").
		Where("kms_provider <> ? OR kms_key_id <> ? OR kms_provider IS NULL OR kms_key_id IS NULL", primaryProvider, primaryKeyID)
}
