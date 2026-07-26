package workerfacts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	StateIdle       = "idle"
	StateActive     = "active"
	StateDraining   = "draining"
	StateOffline    = "offline"
	StateTerminated = "terminated"
)

type TargetSnapshot struct {
	TenantID *uuid.UUID
}

type PoolSnapshot struct {
	ID            uuid.UUID
	Version       int64
	Mode          string
	CapacityClass string
	Region        string
}

type RequestedResources struct {
	CPUMillicores         *int64
	MemoryBytes           *int64
	EphemeralStorageBytes *int64
}

type EnsureInput struct {
	Worker                persistence.WorkerInstance
	Target                TargetSnapshot
	Pool                  *PoolSnapshot
	RequestedResources    RequestedResources
	ObservedAt            time.Time
	InitialState          string
	InitialTerminalReason *string
}

type TransitionInput struct {
	EnsureInput
	NextState           string
	IncrementClaimCount bool
	TerminalReason      *string
}

func EnsureCurrentInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	input EnsureInput,
) (persistence.WorkerIncarnationFact, error) {
	expected, initialState, err := expectedFactForEnsure(input)
	if err != nil {
		return persistence.WorkerIncarnationFact{}, err
	}

	fact, err := lockCurrentFact(ctx, tx, input.Worker.ID, input.Worker.Incarnation)
	if err == nil {
		if err := validateIdentity(fact, expected); err != nil {
			return persistence.WorkerIncarnationFact{}, err
		}
		return fact, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerIncarnationFact{}, problem.Wrap(500, "worker_fact_load_failed", "The Worker incarnation fact could not be loaded.", err)
	}

	model := expected
	model.CurrentState = initialState
	model.StateChangedAt = input.ObservedAt.UTC()
	model.CreatedAt = input.ObservedAt.UTC()
	model.UpdatedAt = input.ObservedAt.UTC()
	if initialState == StateTerminated {
		model.TerminatedAt = timePointer(input.ObservedAt.UTC())
		model.TerminalReason = normalizeTerminalReason(input.InitialTerminalReason)
	}

	if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
		reloaded, reloadErr := lockCurrentFact(ctx, tx, input.Worker.ID, input.Worker.Incarnation)
		if reloadErr == nil {
			if identityErr := validateIdentity(reloaded, expected); identityErr != nil {
				return persistence.WorkerIncarnationFact{}, identityErr
			}
			return reloaded, nil
		}
		return persistence.WorkerIncarnationFact{}, problem.Wrap(409, "worker_fact_create_rejected", "The Worker incarnation fact could not be created.", err)
	}
	return model, nil
}

func TransitionCurrentInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	input TransitionInput,
) (persistence.WorkerIncarnationFact, error) {
	nextState, err := normalizeState(input.NextState)
	if err != nil {
		return persistence.WorkerIncarnationFact{}, err
	}
	terminalReason, err := validateTransitionTerminalReason(nextState, input.TerminalReason)
	if err != nil {
		return persistence.WorkerIncarnationFact{}, err
	}

	ensureInput := input.EnsureInput
	ensureInput.InitialState = nextState
	ensureInput.InitialTerminalReason = terminalReason
	fact, err := EnsureCurrentInTransaction(ctx, tx, ensureInput)
	if err != nil {
		return persistence.WorkerIncarnationFact{}, err
	}

	now := input.ObservedAt.UTC()
	if now.Before(fact.StateChangedAt) || now.Before(fact.UpdatedAt) {
		return persistence.WorkerIncarnationFact{}, problem.New(409, "worker_fact_clock_regressed", "The Worker incarnation fact timestamp moved backwards.")
	}

	if fact.CurrentState == StateTerminated {
		if nextState != StateTerminated || input.IncrementClaimCount || !sameOptionalString(fact.TerminalReason, terminalReason) {
			return persistence.WorkerIncarnationFact{}, problem.New(409, "worker_fact_terminalized", "The Worker incarnation fact is already terminal.")
		}
		return fact, nil
	}

	if fact.CurrentState == nextState {
		if !input.IncrementClaimCount {
			return fact, nil
		}
		return applyFactUpdate(ctx, tx, fact, map[string]any{
			"claim_count": fact.ClaimCount + 1,
			"updated_at":  now,
		})
	}

	accumulatedActive := fact.AccumulatedActiveSeconds
	accumulatedIdle := fact.AccumulatedIdleSeconds
	elapsedWholeSeconds := wholeSecondsBetween(fact.StateChangedAt, now)
	switch fact.CurrentState {
	case StateActive:
		accumulatedActive += elapsedWholeSeconds
	case StateIdle:
		accumulatedIdle += elapsedWholeSeconds
	}

	updates := map[string]any{
		"current_state":              nextState,
		"state_changed_at":           now,
		"updated_at":                 now,
		"accumulated_active_seconds": accumulatedActive,
		"accumulated_idle_seconds":   accumulatedIdle,
	}
	if input.IncrementClaimCount {
		updates["claim_count"] = fact.ClaimCount + 1
	}
	if nextState == StateTerminated {
		updates["terminated_at"] = now
		updates["terminal_reason"] = terminalReason
	}
	return applyFactUpdate(ctx, tx, fact, updates)
}

func applyFactUpdate(
	ctx context.Context,
	tx *gorm.DB,
	current persistence.WorkerIncarnationFact,
	updates map[string]any,
) (persistence.WorkerIncarnationFact, error) {
	result := tx.WithContext(ctx).
		Model(&persistence.WorkerIncarnationFact{}).
		Where("worker_id = ? AND worker_incarnation = ? AND updated_at = ?",
			current.WorkerID, current.WorkerIncarnation, current.UpdatedAt,
		).
		Updates(updates)
	if result.Error != nil {
		return persistence.WorkerIncarnationFact{}, problem.Wrap(500, "worker_fact_update_failed", "The Worker incarnation fact could not be updated.", result.Error)
	}
	if result.RowsAffected != 1 {
		return persistence.WorkerIncarnationFact{}, problem.New(409, "worker_fact_update_conflict", "The Worker incarnation fact changed concurrently.")
	}
	return lockCurrentFact(ctx, tx, current.WorkerID, current.WorkerIncarnation)
}

func expectedFactForEnsure(input EnsureInput) (persistence.WorkerIncarnationFact, string, error) {
	if err := validateEnsureInput(input); err != nil {
		return persistence.WorkerIncarnationFact{}, "", err
	}
	initialState := deriveInitialState(input)
	terminalReason := normalizeTerminalReason(input.InitialTerminalReason)
	if initialState != StateTerminated {
		terminalReason = nil
	}

	expected := persistence.WorkerIncarnationFact{
		WorkerID:                       input.Worker.ID,
		WorkerIncarnation:              input.Worker.Incarnation,
		TenantID:                       input.Target.TenantID,
		ExecutionTargetID:              input.Worker.ExecutionTargetID,
		TargetKind:                     input.Worker.TargetKind,
		WorkerMode:                     input.Worker.WorkerMode,
		WorkerPoolID:                   input.Worker.WorkerPoolID,
		WorkerPoolVersion:              input.Worker.WorkerPoolVersion,
		ClusterID:                      input.Worker.ClusterID,
		Region:                         "",
		Namespace:                      input.Worker.Namespace,
		PodName:                        input.Worker.PodName,
		InstanceUID:                    input.Worker.InstanceUID,
		RegisteredAt:                   input.Worker.RegisteredAt.UTC(),
		RequestedCPUMillicores:         cloneInt64Pointer(input.RequestedResources.CPUMillicores),
		RequestedMemoryBytes:           cloneInt64Pointer(input.RequestedResources.MemoryBytes),
		RequestedEphemeralStorageBytes: cloneInt64Pointer(input.RequestedResources.EphemeralStorageBytes),
	}
	if input.Pool != nil {
		expected.WorkerPoolID = uuidPointer(input.Pool.ID)
		// The Worker registration freezes the exact Pool version. The Pool row
		// may advance before a later lifecycle transition, so never rewrite the
		// physical incarnation fact to the Pool's current version.
		expected.WorkerPoolVersion = cloneInt64Pointer(input.Worker.WorkerPoolVersion)
		expected.PoolMode = stringPointer(input.Pool.Mode)
		expected.CapacityClass = stringPointer(input.Pool.CapacityClass)
		expected.Region = input.Pool.Region
	}
	if input.Worker.CapacityClass != nil && expected.CapacityClass == nil {
		expected.CapacityClass = stringPointer(*input.Worker.CapacityClass)
	}
	if initialState == StateTerminated {
		expected.TerminalReason = terminalReason
	}
	return expected, initialState, nil
}

func validateEnsureInput(input EnsureInput) error {
	if input.Worker.ID == uuid.Nil {
		return problem.New(400, "invalid_worker_fact_snapshot", "Worker id is required.")
	}
	if input.Worker.Incarnation <= 0 {
		return problem.New(400, "invalid_worker_fact_snapshot", "Worker incarnation must be positive.")
	}
	if input.Worker.ExecutionTargetID == uuid.Nil {
		return problem.New(400, "invalid_worker_fact_snapshot", "Worker executionTargetId is required.")
	}
	if input.ObservedAt.IsZero() {
		return problem.New(400, "invalid_worker_fact_snapshot", "observedAt is required.")
	}
	if input.ObservedAt.UTC().Before(input.Worker.RegisteredAt.UTC()) {
		return problem.New(409, "worker_fact_clock_regressed", "The Worker incarnation fact timestamp moved backwards.")
	}
	if _, err := normalizeState(deriveInitialState(input)); err != nil {
		return err
	}
	if err := validatePoolSnapshot(input.Worker, input.Pool); err != nil {
		return err
	}
	if err := validateRequestedResources(input.RequestedResources); err != nil {
		return err
	}
	if _, err := validateTransitionTerminalReason(deriveInitialState(input), input.InitialTerminalReason); err != nil {
		return err
	}
	return nil
}

func validatePoolSnapshot(worker persistence.WorkerInstance, pool *PoolSnapshot) error {
	if worker.WorkerPoolID == nil {
		if pool != nil || worker.WorkerPoolVersion != nil || worker.CapacityClass != nil {
			return problem.New(409, "worker_fact_snapshot_conflict", "The Worker pool snapshot is inconsistent with the persisted Worker.")
		}
		return nil
	}
	if pool == nil || worker.WorkerPoolVersion == nil || worker.CapacityClass == nil {
		return problem.New(409, "worker_fact_snapshot_conflict", "The Worker pool snapshot is incomplete for the persisted Worker.")
	}
	if pool.ID == uuid.Nil || pool.ID != *worker.WorkerPoolID || pool.Version < *worker.WorkerPoolVersion {
		return problem.New(409, "worker_fact_snapshot_conflict", "The Worker pool snapshot does not match the persisted Worker.")
	}
	if strings.TrimSpace(pool.CapacityClass) != strings.TrimSpace(*worker.CapacityClass) {
		return problem.New(409, "worker_fact_snapshot_conflict", "The Worker capacity class snapshot does not match the persisted Worker.")
	}
	if _, err := normalizePoolMode(pool.Mode); err != nil {
		return err
	}
	if _, err := normalizeCapacityClass(pool.CapacityClass); err != nil {
		return err
	}
	return nil
}

func validateRequestedResources(resources RequestedResources) error {
	for _, resource := range []struct {
		value *int64
		name  string
	}{
		{value: resources.CPUMillicores, name: "requestedCpuMillicores"},
		{value: resources.MemoryBytes, name: "requestedMemoryBytes"},
		{value: resources.EphemeralStorageBytes, name: "requestedEphemeralStorageBytes"},
	} {
		if resource.value != nil && *resource.value <= 0 {
			return problem.New(400, "invalid_worker_fact_snapshot", resource.name+" must be positive when present.")
		}
	}
	return nil
}

func deriveInitialState(input EnsureInput) string {
	if strings.TrimSpace(input.InitialState) != "" {
		return strings.TrimSpace(input.InitialState)
	}
	switch strings.TrimSpace(input.Worker.Status) {
	case "draining":
		return StateDraining
	case "offline":
		return StateOffline
	case "terminated":
		return StateTerminated
	default:
		return StateIdle
	}
}

func lockCurrentFact(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
) (persistence.WorkerIncarnationFact, error) {
	var fact persistence.WorkerIncarnationFact
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("worker_id = ? AND worker_incarnation = ?", workerID, workerIncarnation).
		Take(&fact).Error
	return fact, err
}

func validateIdentity(current, expected persistence.WorkerIncarnationFact) error {
	switch {
	case current.WorkerID != expected.WorkerID,
		current.WorkerIncarnation != expected.WorkerIncarnation,
		!sameOptionalUUID(current.TenantID, expected.TenantID),
		current.ExecutionTargetID != expected.ExecutionTargetID,
		current.TargetKind != expected.TargetKind,
		current.WorkerMode != expected.WorkerMode,
		!sameOptionalUUID(current.WorkerPoolID, expected.WorkerPoolID),
		!sameOptionalInt64(current.WorkerPoolVersion, expected.WorkerPoolVersion),
		!sameOptionalString(current.PoolMode, expected.PoolMode),
		!sameOptionalString(current.CapacityClass, expected.CapacityClass),
		current.ClusterID != expected.ClusterID,
		// Region can change on the live Pool row after registration. The fact's
		// region remains the registration-time snapshot and is therefore not
		// revalidated from mutable Pool state on every transition.
		current.Namespace != expected.Namespace,
		current.PodName != expected.PodName,
		current.InstanceUID != expected.InstanceUID,
		!current.RegisteredAt.Equal(expected.RegisteredAt),
		!sameOptionalInt64(current.RequestedCPUMillicores, expected.RequestedCPUMillicores),
		!sameOptionalInt64(current.RequestedMemoryBytes, expected.RequestedMemoryBytes),
		!sameOptionalInt64(current.RequestedEphemeralStorageBytes, expected.RequestedEphemeralStorageBytes):
		return problem.New(409, "worker_fact_identity_conflict", "The Worker incarnation fact is already bound to a different immutable identity.")
	default:
		return nil
	}
}

func validateTransitionTerminalReason(nextState string, reason *string) (*string, error) {
	normalized := normalizeTerminalReason(reason)
	if nextState != StateTerminated && normalized != nil {
		return nil, problem.New(400, "invalid_worker_fact_terminal_reason", "terminalReason is allowed only for the terminated Worker state.")
	}
	if normalized != nil && len(*normalized) > 160 {
		return nil, problem.New(400, "invalid_worker_fact_terminal_reason", "terminalReason must not exceed 160 characters.")
	}
	return normalized, nil
}

func normalizeState(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case StateIdle, StateActive, StateDraining, StateOffline, StateTerminated:
		return strings.TrimSpace(value), nil
	default:
		return "", problem.New(400, "invalid_worker_fact_state", "Worker incarnation fact state is invalid.")
	}
}

func normalizePoolMode(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "resident", "per-execution", "warm":
		return strings.TrimSpace(value), nil
	default:
		return "", problem.New(400, "invalid_worker_fact_snapshot", "Worker pool mode snapshot is invalid.")
	}
}

func normalizeCapacityClass(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "standard", "interactive":
		return strings.TrimSpace(value), nil
	default:
		return "", problem.New(400, "invalid_worker_fact_snapshot", "Worker capacity class snapshot is invalid.")
	}
}

func wholeSecondsBetween(start, end time.Time) int64 {
	if end.Before(start) {
		return 0
	}
	return int64(end.Sub(start) / time.Second)
}

func normalizeTerminalReason(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func timePointer(value time.Time) *time.Time { return &value }

func uuidPointer(value uuid.UUID) *uuid.UUID { return &value }

func stringPointer(value string) *string { return &value }

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
