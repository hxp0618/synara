package persistence

import (
	"time"

	"github.com/google/uuid"
)

// ExecutionRuntimeIsolationDecision freezes the runtime and outer isolation
// profile selected before one Execution Generation creates external resources.
// Rows are append-only and scoped to the same immutable Generation authority as
// execution_generation_facts.
type ExecutionRuntimeIsolationDecision struct {
	TenantID             uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey;not null"`
	ExecutionID          uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey;not null"`
	Generation           int64      `gorm:"column:generation;primaryKey;not null"`
	ExecutionTargetID    uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null;index:idx_execution_runtime_isolation_target_profile,priority:1"`
	AllocationBackend    string     `gorm:"column:allocation_backend;not null"`
	RequestedRuntime     string     `gorm:"column:requested_runtime;not null"`
	RequestedProfile     string     `gorm:"column:requested_profile;not null"`
	EffectiveRuntime     *string    `gorm:"column:effective_runtime"`
	EffectiveProfile     *string    `gorm:"column:effective_profile;index:idx_execution_runtime_isolation_target_profile,priority:2"`
	PolicySource         string     `gorm:"column:policy_source;not null"`
	Decision             string     `gorm:"column:decision;not null"`
	DecisionReasonCode   *string    `gorm:"column:decision_reason_code"`
	RuntimeClassName     *string    `gorm:"column:runtime_class_name"`
	AttestationDigest    *string    `gorm:"column:attestation_digest"`
	AttestedAt           *time.Time `gorm:"column:attested_at"`
	AttestationExpiresAt *time.Time `gorm:"column:attestation_expires_at"`
	CreatedAt            time.Time  `gorm:"column:created_at;not null"`
}

func (ExecutionRuntimeIsolationDecision) TableName() string {
	return "execution_runtime_isolation_decisions"
}

// ExecutionTargetRuntimeIsolationObservation is the bounded, replaceable
// capability view most recently observed from one Target. It deliberately
// excludes Node names, host paths, sockets, and raw attestation payloads.
type ExecutionTargetRuntimeIsolationObservation struct {
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id;type:uuid;primaryKey"`
	DetectedRuntimes  []string  `gorm:"column:detected_runtimes;serializer:json;not null"`
	DetectedProfiles  []string  `gorm:"column:detected_profiles;serializer:json;not null"`
	RequestedRuntime  string    `gorm:"column:requested_runtime;not null"`
	RequestedProfile  string    `gorm:"column:requested_profile;not null"`
	EffectiveRuntime  *string   `gorm:"column:effective_runtime"`
	EffectiveProfile  *string   `gorm:"column:effective_profile"`
	PolicySource      string    `gorm:"column:policy_source;not null"`
	Decision          string    `gorm:"column:decision;not null"`
	State             string    `gorm:"column:state;not null"`
	ReasonCode        *string   `gorm:"column:reason_code"`
	ObservedAt        time.Time `gorm:"column:observed_at;not null"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null"`
}

func (ExecutionTargetRuntimeIsolationObservation) TableName() string {
	return "execution_target_runtime_isolation_observations"
}
