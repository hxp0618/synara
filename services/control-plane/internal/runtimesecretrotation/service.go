package runtimesecretrotation

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
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const (
	ResourceExecutionTargetConfiguration = "execution_target_configuration"
	ResourceProviderResumeCursor         = "provider_resume_cursor"
	defaultBatchSize                     = 200
	maxBatchSize                         = 1000
	maximumConvergencePasses             = 3
)

type Service struct {
	db     *gorm.DB
	cipher *secret.CursorCipher
	now    func() time.Time
}

type InventoryEntry struct {
	ResourceType string `json:"resourceType"`
	KeyID        string `json:"keyId"`
	Count        int64  `json:"count"`
	Primary      bool   `json:"primary"`
	Keyed        bool   `json:"keyed"`
	Decryptable  bool   `json:"decryptable"`
}

type Plan struct {
	PrimaryKeyID          string           `json:"primaryKeyId"`
	Inventory             []InventoryEntry `json:"inventory"`
	AlreadyPrimary        int64            `json:"alreadyPrimary"`
	PendingReencrypt      int64            `json:"pendingReencrypt"`
	Unconfigured          int64            `json:"unconfigured"`
	IgnoredUnusableCursor int64            `json:"ignoredUnusableCursor"`
}

type ExecuteOptions struct {
	OperatorReference string
	BatchSize         int
	ResumeRunID       *uuid.UUID
}

type Report struct {
	RunID                     uuid.UUID `json:"runId"`
	PrimaryKeyID              string    `json:"primaryKeyId"`
	ExecutionTargetCount      int64     `json:"executionTargetCount"`
	ProviderResumeCursorCount int64     `json:"providerResumeCursorCount"`
	EntryDigest               string    `json:"entryDigest"`
	CompletedAt               time.Time `json:"completedAt"`
}

type inspection struct {
	keyID       string
	keyed       bool
	primary     bool
	decryptable bool
	plaintext   []byte
}

type inventoryKey struct {
	resourceType string
	keyID        string
	keyed        bool
	primary      bool
	decryptable  bool
}

func New(db *gorm.DB, cipher *secret.CursorCipher) (*Service, error) {
	if db == nil {
		return nil, errors.New("runtime Secret rotation database is required")
	}
	if cipher == nil || strings.TrimSpace(cipher.PrimaryKeyID()) == "" {
		return nil, errors.New("runtime Secret rotation requires an explicitly named primary key")
	}
	return &Service{db: db, cipher: cipher, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Plan(ctx context.Context, batchSize int) (Plan, error) {
	batchSize, err := normalizeBatchSize(batchSize)
	if err != nil {
		return Plan{}, err
	}
	result := Plan{PrimaryKeyID: s.cipher.PrimaryKeyID()}
	inventory := map[inventoryKey]int64{}
	if err := s.eachTarget(ctx, batchSize, func(model persistence.ExecutionTarget) error {
		inspected := s.inspectTarget(model)
		defer zero(inspected.plaintext)
		appendInspection(&result, inventory, ResourceExecutionTargetConfiguration, inspected)
		return nil
	}); err != nil {
		return Plan{}, err
	}
	if err := s.countIgnoredCursors(ctx, &result.IgnoredUnusableCursor); err != nil {
		return Plan{}, err
	}
	if err := s.eachUsableCursor(ctx, batchSize, func(
		session persistence.AgentSession,
		execution persistence.AgentExecution,
	) error {
		inspected := s.inspectCursor(session, execution)
		defer zero(inspected.plaintext)
		appendInspection(&result, inventory, ResourceProviderResumeCursor, inspected)
		return nil
	}); err != nil {
		return Plan{}, err
	}
	for key, count := range inventory {
		result.Inventory = append(result.Inventory, InventoryEntry{
			ResourceType: key.resourceType, KeyID: key.keyID, Count: count,
			Primary: key.primary, Keyed: key.keyed, Decryptable: key.decryptable,
		})
	}
	sort.Slice(result.Inventory, func(left, right int) bool {
		if result.Inventory[left].ResourceType != result.Inventory[right].ResourceType {
			return result.Inventory[left].ResourceType < result.Inventory[right].ResourceType
		}
		if result.Inventory[left].KeyID != result.Inventory[right].KeyID {
			return result.Inventory[left].KeyID < result.Inventory[right].KeyID
		}
		if result.Inventory[left].Keyed != result.Inventory[right].Keyed {
			return !result.Inventory[left].Keyed
		}
		return !result.Inventory[left].Decryptable && result.Inventory[right].Decryptable
	})
	return result, nil
}

func appendInspection(
	result *Plan,
	inventory map[inventoryKey]int64,
	resourceType string,
	inspected inspection,
) {
	keyID := inspected.keyID
	if keyID == "" {
		keyID = "unresolved"
	}
	converged := inspected.decryptable && inspected.primary && inspected.keyed
	inventory[inventoryKey{
		resourceType: resourceType, keyID: keyID, keyed: inspected.keyed,
		primary: converged, decryptable: inspected.decryptable,
	}]++
	if converged {
		result.AlreadyPrimary++
		return
	}
	result.PendingReencrypt++
	if !inspected.decryptable {
		result.Unconfigured++
	}
}

func (s *Service) Execute(ctx context.Context, options ExecuteOptions) (Report, error) {
	operatorReference := strings.TrimSpace(options.OperatorReference)
	if len(operatorReference) < 3 || len(operatorReference) > 200 || strings.ContainsAny(operatorReference, "\r\n\t") {
		return Report{}, errors.New("runtime Secret rotation operator reference must contain 3 to 200 single-line characters")
	}
	batchSize, err := normalizeBatchSize(options.BatchSize)
	if err != nil {
		return Report{}, err
	}
	plan, err := s.Plan(ctx, batchSize)
	if err != nil {
		return Report{}, err
	}
	if plan.Unconfigured > 0 {
		return Report{}, fmt.Errorf(
			"runtime Secret rotation cannot decrypt %d usable resources; configure every old key first",
			plan.Unconfigured,
		)
	}
	run, err := s.resolveRun(ctx, operatorReference, options.ResumeRunID)
	if err != nil {
		return Report{}, err
	}
	for pass := 0; pass < maximumConvergencePasses; pass++ {
		if err := s.reencryptTargets(ctx, run, batchSize); err != nil {
			return Report{}, err
		}
		if err := s.reencryptCursors(ctx, run, batchSize); err != nil {
			return Report{}, err
		}
		remaining, err := s.Plan(ctx, batchSize)
		if err != nil {
			return Report{}, err
		}
		if remaining.Unconfigured > 0 {
			return Report{}, fmt.Errorf("runtime Secret rotation encountered %d unreadable resources", remaining.Unconfigured)
		}
		if remaining.PendingReencrypt == 0 {
			return s.complete(ctx, run)
		}
	}
	return Report{}, errors.New("runtime Secret rotation did not converge after concurrent mutations")
}

func (s *Service) resolveRun(
	ctx context.Context,
	operatorReference string,
	resumeRunID *uuid.UUID,
) (persistence.RuntimeSecretRekeyRun, error) {
	if resumeRunID == nil {
		run := persistence.RuntimeSecretRekeyRun{
			ID: uuid.New(), PrimaryKeyID: s.cipher.PrimaryKeyID(),
			OperatorReference: operatorReference, StartedAt: s.now(),
		}
		if err := s.db.WithContext(ctx).Create(&run).Error; err != nil {
			return persistence.RuntimeSecretRekeyRun{}, fmt.Errorf("create runtime Secret rekey run: %w", err)
		}
		return run, nil
	}
	var run persistence.RuntimeSecretRekeyRun
	if err := s.db.WithContext(ctx).First(&run, "id = ?", *resumeRunID).Error; err != nil {
		return persistence.RuntimeSecretRekeyRun{}, fmt.Errorf("load runtime Secret rekey run: %w", err)
	}
	if run.PrimaryKeyID != s.cipher.PrimaryKeyID() {
		return persistence.RuntimeSecretRekeyRun{}, errors.New("runtime Secret rekey run targets a different primary key")
	}
	if run.OperatorReference != operatorReference {
		return persistence.RuntimeSecretRekeyRun{}, errors.New("runtime Secret rekey run operator reference does not match")
	}
	var receiptCount int64
	if err := s.db.WithContext(ctx).Model(&persistence.RuntimeSecretRekeyReceipt{}).
		Where("run_id = ?", run.ID).Count(&receiptCount).Error; err != nil {
		return persistence.RuntimeSecretRekeyRun{}, fmt.Errorf("inspect runtime Secret rekey receipt: %w", err)
	}
	if receiptCount != 0 {
		return persistence.RuntimeSecretRekeyRun{}, errors.New("runtime Secret rekey run is already complete")
	}
	return run, nil
}

func (s *Service) inspectTarget(model persistence.ExecutionTarget) inspection {
	plain, metadata, err := s.cipher.DecryptBytesWithMetadata(model.ConfigurationEncrypted)
	if err != nil || !storedKeyMatches(model.ConfigurationKeyID, metadata) {
		return inspection{}
	}
	return inspection{
		keyID: metadata.KeyID, keyed: metadata.Keyed,
		primary:     metadata.Primary && storedPrimary(model.ConfigurationKeyID, s.cipher.PrimaryKeyID()),
		decryptable: true, plaintext: plain,
	}
}

func (s *Service) inspectCursor(
	session persistence.AgentSession,
	execution persistence.AgentExecution,
) inspection {
	binding, ok := executions.ProviderCursorBindingFromExecution(execution)
	if !ok {
		return inspection{}
	}
	plain, status, metadata, err := s.cipher.OpenV2WithMetadata(
		session.ProviderResumeCursorEncrypted, binding.Version, binding.Digest,
	)
	if err != nil || status != secret.CursorOpenValid || !storedKeyMatches(session.ProviderResumeCursorKeyID, metadata) {
		return inspection{}
	}
	return inspection{
		keyID: metadata.KeyID, keyed: metadata.Keyed,
		primary:     metadata.Primary && storedPrimary(session.ProviderResumeCursorKeyID, s.cipher.PrimaryKeyID()),
		decryptable: true, plaintext: plain,
	}
}

func storedKeyMatches(stored *string, metadata secret.DecryptionMetadata) bool {
	if stored == nil {
		return !metadata.Keyed
	}
	return metadata.Keyed && *stored == metadata.KeyID
}

func storedPrimary(stored *string, primaryKeyID string) bool {
	return stored != nil && *stored == primaryKeyID
}

func (s *Service) reencryptTargets(
	ctx context.Context,
	run persistence.RuntimeSecretRekeyRun,
	batchSize int,
) error {
	return s.eachTarget(ctx, batchSize, func(model persistence.ExecutionTarget) error {
		inspected := s.inspectTarget(model)
		defer zero(inspected.plaintext)
		if !inspected.decryptable {
			return fmt.Errorf("Execution Target %s configuration is unreadable or its stored key ID is inconsistent", model.ID)
		}
		if inspected.primary && inspected.keyed {
			return nil
		}
		replacement, err := s.cipher.EncryptBytes(inspected.plaintext)
		if err != nil {
			return fmt.Errorf("reencrypt Execution Target %s configuration: %w", model.ID, err)
		}
		return s.persistRekey(ctx, run, rekeyMutation{
			resourceType: ResourceExecutionTargetConfiguration, resourceID: model.ID, tenantID: model.TenantID,
			oldStoredKeyID: model.ConfigurationKeyID, decryptKeyID: inspected.keyID,
			oldCiphertext: model.ConfigurationEncrypted, newCiphertext: replacement,
			update: func(tx *gorm.DB) (int64, error) {
				query := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
					Where("id = ? AND configuration_encrypted = ?", model.ID, model.ConfigurationEncrypted)
				query = whereNullableKeyID(query, "configuration_key_id", model.ConfigurationKeyID)
				result := query.UpdateColumns(map[string]any{
					"configuration_encrypted": replacement,
					"configuration_key_id":    s.cipher.PrimaryKeyID(),
				})
				return result.RowsAffected, result.Error
			},
		})
	})
}

func (s *Service) reencryptCursors(
	ctx context.Context,
	run persistence.RuntimeSecretRekeyRun,
	batchSize int,
) error {
	return s.eachUsableCursor(ctx, batchSize, func(
		session persistence.AgentSession,
		execution persistence.AgentExecution,
	) error {
		inspected := s.inspectCursor(session, execution)
		defer zero(inspected.plaintext)
		if !inspected.decryptable {
			return fmt.Errorf("Session %s Provider Cursor is unreadable or its stored key ID is inconsistent", session.ID)
		}
		if inspected.primary && inspected.keyed {
			return nil
		}
		binding, _ := executions.ProviderCursorBindingFromExecution(execution)
		replacement, err := s.cipher.SealV2(inspected.plaintext, binding.Version, binding.Digest)
		if err != nil {
			return fmt.Errorf("reencrypt Session %s Provider Cursor: %w", session.ID, err)
		}
		tenantID := session.TenantID
		return s.persistRekey(ctx, run, rekeyMutation{
			resourceType: ResourceProviderResumeCursor, resourceID: session.ID, tenantID: &tenantID,
			oldStoredKeyID: session.ProviderResumeCursorKeyID, decryptKeyID: inspected.keyID,
			oldCiphertext: session.ProviderResumeCursorEncrypted, newCiphertext: replacement,
			update: func(tx *gorm.DB) (int64, error) {
				query := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
					Where("tenant_id = ? AND id = ? AND provider_resume_cursor_state = ? AND provider_resume_cursor_encrypted = ?",
						session.TenantID, session.ID, "usable", session.ProviderResumeCursorEncrypted)
				query = whereNullableKeyID(query, "provider_resume_cursor_key_id", session.ProviderResumeCursorKeyID)
				result := query.UpdateColumns(map[string]any{
					"provider_resume_cursor_encrypted": replacement,
					"provider_resume_cursor_key_id":    s.cipher.PrimaryKeyID(),
				})
				return result.RowsAffected, result.Error
			},
		})
	})
}

type rekeyMutation struct {
	resourceType   string
	resourceID     uuid.UUID
	tenantID       *uuid.UUID
	oldStoredKeyID *string
	decryptKeyID   string
	oldCiphertext  []byte
	newCiphertext  []byte
	update         func(*gorm.DB) (int64, error)
}

func (s *Service) persistRekey(
	ctx context.Context,
	run persistence.RuntimeSecretRekeyRun,
	mutation rekeyMutation,
) error {
	oldDigest := sha256.Sum256(mutation.oldCiphertext)
	newDigest := sha256.Sum256(mutation.newCiphertext)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		entry := persistence.RuntimeSecretRekeyEntry{
			ID: uuid.New(), RunID: run.ID, TenantID: mutation.tenantID,
			ResourceType: mutation.resourceType, ResourceID: mutation.resourceID,
			OldStoredKeyID: mutation.oldStoredKeyID, DecryptKeyID: mutation.decryptKeyID,
			NewKeyID: s.cipher.PrimaryKeyID(), OldCiphertextSHA256: oldDigest[:],
			NewCiphertextSHA256: newDigest[:], CreatedAt: s.now(),
		}
		if err := tx.Create(&entry).Error; err != nil {
			return fmt.Errorf("record runtime Secret rekey evidence: %w", err)
		}
		rows, err := mutation.update(tx)
		if err != nil {
			return err
		}
		if rows != 1 {
			return errConcurrentMutation
		}
		if mutation.tenantID == nil {
			return nil
		}
		resourceID := mutation.resourceID
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *mutation.tenantID, ActorType: "system",
			Action: "security.runtime_secret_reencrypted", ResourceType: mutation.resourceType,
			ResourceID: &resourceID, RequestID: run.OperatorReference,
			Metadata: map[string]any{
				"runId": run.ID, "decryptKeyId": mutation.decryptKeyID,
				"fromStoredKeyId": mutation.oldStoredKeyID, "toKeyId": s.cipher.PrimaryKeyID(),
			},
		})
	})
	if errors.Is(err, errConcurrentMutation) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("persist runtime Secret rekey for %s %s: %w", mutation.resourceType, mutation.resourceID, err)
	}
	return nil
}

var errConcurrentMutation = errors.New("runtime Secret changed during re-encryption")

func (s *Service) complete(
	ctx context.Context,
	run persistence.RuntimeSecretRekeyRun,
) (Report, error) {
	var entries []persistence.RuntimeSecretRekeyEntry
	if err := s.db.WithContext(ctx).Where("run_id = ?", run.ID).
		Order("resource_type ASC, resource_id ASC, id ASC").Find(&entries).Error; err != nil {
		return Report{}, fmt.Errorf("load runtime Secret rekey evidence: %w", err)
	}
	hash := sha256.New()
	counts := map[string]int64{}
	for _, entry := range entries {
		counts[entry.ResourceType]++
		for _, value := range [][]byte{
			entry.ID[:], []byte(entry.ResourceType), entry.ResourceID[:],
			[]byte(nullableString(entry.OldStoredKeyID)), []byte(entry.DecryptKeyID), []byte(entry.NewKeyID),
			entry.OldCiphertextSHA256, entry.NewCiphertextSHA256,
		} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write(value)
		}
	}
	digest := hash.Sum(nil)
	receipt := persistence.RuntimeSecretRekeyReceipt{
		RunID:                     run.ID,
		ExecutionTargetCount:      counts[ResourceExecutionTargetConfiguration],
		ProviderResumeCursorCount: counts[ResourceProviderResumeCursor],
		EntryDigest:               digest, CompletedAt: s.now(),
	}
	if err := s.db.WithContext(ctx).Create(&receipt).Error; err != nil {
		return Report{}, fmt.Errorf("record runtime Secret rekey receipt: %w", err)
	}
	return Report{
		RunID: run.ID, PrimaryKeyID: run.PrimaryKeyID,
		ExecutionTargetCount:      receipt.ExecutionTargetCount,
		ProviderResumeCursorCount: receipt.ProviderResumeCursorCount,
		EntryDigest:               fmt.Sprintf("%x", digest), CompletedAt: receipt.CompletedAt,
	}, nil
}

func (s *Service) eachTarget(
	ctx context.Context,
	batchSize int,
	visit func(persistence.ExecutionTarget) error,
) error {
	lastID := uuid.Nil
	for {
		var models []persistence.ExecutionTarget
		query := s.db.WithContext(ctx).Where("length(configuration_encrypted) > 0")
		if lastID != uuid.Nil {
			query = query.Where("id > ?", lastID)
		}
		if err := query.Order("id ASC").Limit(batchSize).Find(&models).Error; err != nil {
			return fmt.Errorf("load Execution Target runtime Secret batch: %w", err)
		}
		if len(models) == 0 {
			return nil
		}
		for _, model := range models {
			if err := visit(model); err != nil {
				return err
			}
			lastID = model.ID
		}
	}
}

func (s *Service) eachUsableCursor(
	ctx context.Context,
	batchSize int,
	visit func(persistence.AgentSession, persistence.AgentExecution) error,
) error {
	lastID := uuid.Nil
	for {
		var sessions []persistence.AgentSession
		query := s.db.WithContext(ctx).
			Where("provider_resume_cursor_state = ? AND length(provider_resume_cursor_encrypted) > 0", "usable")
		if lastID != uuid.Nil {
			query = query.Where("id > ?", lastID)
		}
		if err := query.Order("id ASC").Limit(batchSize).Find(&sessions).Error; err != nil {
			return fmt.Errorf("load Provider Cursor runtime Secret batch: %w", err)
		}
		if len(sessions) == 0 {
			return nil
		}
		sourceIDs := make([]uuid.UUID, 0, len(sessions))
		for _, session := range sessions {
			if session.ProviderResumeCursorSourceExecutionID == nil || session.ProviderResumeCursorSourceGeneration == nil {
				return fmt.Errorf("Session %s usable Provider Cursor has no source Execution lineage", session.ID)
			}
			sourceIDs = append(sourceIDs, *session.ProviderResumeCursorSourceExecutionID)
		}
		var executionsByID []persistence.AgentExecution
		if err := s.db.WithContext(ctx).Where("id IN ?", sourceIDs).Find(&executionsByID).Error; err != nil {
			return fmt.Errorf("load Provider Cursor source Execution batch: %w", err)
		}
		executionIndex := make(map[uuid.UUID]persistence.AgentExecution, len(executionsByID))
		for _, execution := range executionsByID {
			executionIndex[execution.ID] = execution
		}
		for _, session := range sessions {
			execution, exists := executionIndex[*session.ProviderResumeCursorSourceExecutionID]
			if !exists || execution.TenantID != session.TenantID || execution.SessionID != session.ID ||
				execution.Generation != *session.ProviderResumeCursorSourceGeneration {
				return fmt.Errorf("Session %s Provider Cursor source Execution lineage is inconsistent", session.ID)
			}
			if err := visit(session, execution); err != nil {
				return err
			}
			lastID = session.ID
		}
	}
}

func (s *Service) countIgnoredCursors(ctx context.Context, count *int64) error {
	if err := s.db.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("provider_resume_cursor_state <> ? AND length(provider_resume_cursor_encrypted) > 0", "usable").
		Count(count).Error; err != nil {
		return fmt.Errorf("count unusable Provider Cursors: %w", err)
	}
	return nil
}

func whereNullableKeyID(query *gorm.DB, column string, keyID *string) *gorm.DB {
	if keyID == nil {
		return query.Where(column + " IS NULL")
	}
	return query.Where(column+" = ?", *keyID)
}

func normalizeBatchSize(value int) (int, error) {
	if value == 0 {
		return defaultBatchSize, nil
	}
	if value < 1 || value > maxBatchSize {
		return 0, fmt.Errorf("runtime Secret rotation batch size must be between 1 and %d", maxBatchSize)
	}
	return value, nil
}

func nullableString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
