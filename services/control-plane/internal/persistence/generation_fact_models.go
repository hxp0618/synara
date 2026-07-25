package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionGenerationFact struct {
	TenantID                           uuid.UUID  `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID                        uuid.UUID  `gorm:"column:execution_id;type:uuid;primaryKey"`
	Generation                         int64      `gorm:"column:generation;primaryKey"`
	SessionID                          uuid.UUID  `gorm:"column:session_id;type:uuid"`
	TurnID                             uuid.UUID  `gorm:"column:turn_id;type:uuid"`
	ExecutionTargetID                  uuid.UUID  `gorm:"column:execution_target_id;type:uuid"`
	TargetKind                         string     `gorm:"column:target_kind"`
	Provider                           string     `gorm:"column:provider"`
	RecoveryReason                     string     `gorm:"column:recovery_reason"`
	WarmPoolMode                       string     `gorm:"column:warm_pool_mode;default:disabled"`
	WarmPoolResult                     string     `gorm:"column:warm_pool_result;default:pending"`
	DispatchRequestedAt                *time.Time `gorm:"column:dispatch_requested_at"`
	BundleCreatedAt                    *time.Time `gorm:"column:bundle_created_at"`
	LeasedAt                           *time.Time `gorm:"column:leased_at"`
	ExecutionStartedAt                 *time.Time `gorm:"column:execution_started_at"`
	ProviderReadyAt                    *time.Time `gorm:"column:provider_ready_at"`
	TerminalAt                         *time.Time `gorm:"column:terminal_at"`
	TerminalOutcome                    *string    `gorm:"column:terminal_outcome"`
	ProviderResumeStrategy             string     `gorm:"column:provider_resume_strategy;default:authoritative-history"`
	ResumeAttemptedStrategy            *string    `gorm:"column:resume_attempted_strategy"`
	ResumeSelectedStrategy             *string    `gorm:"column:resume_selected_strategy"`
	ResumeFallbackOutcome              *string    `gorm:"column:resume_fallback_outcome"`
	ResumeFallbackReasonCode           *string    `gorm:"column:resume_fallback_reason_code"`
	ResumeFallbackSafety               *string    `gorm:"column:resume_fallback_safety"`
	ResumeFallbackProvider             *string    `gorm:"column:resume_fallback_provider"`
	ResumeAuthoritativeHistorySequence *int64     `gorm:"column:resume_authoritative_history_sequence"`
	CreatedAt                          time.Time  `gorm:"column:created_at"`
	UpdatedAt                          time.Time  `gorm:"column:updated_at"`
}

func (ExecutionGenerationFact) TableName() string { return "execution_generation_facts" }
