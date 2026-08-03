package persistence

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionUsageSummary struct {
	TenantID              uuid.UUID `gorm:"column:tenant_id;type:uuid;primaryKey"`
	ExecutionID           uuid.UUID `gorm:"column:execution_id;type:uuid;primaryKey"`
	Generation            int64     `gorm:"column:generation;primaryKey"`
	SessionID             uuid.UUID `gorm:"column:session_id;type:uuid"`
	TurnID                uuid.UUID `gorm:"column:turn_id;type:uuid"`
	Provider              string    `gorm:"column:provider"`
	Model                 *string   `gorm:"column:model"`
	InputTokens           int64     `gorm:"column:input_tokens"`
	CachedInputTokens     int64     `gorm:"column:cached_input_tokens"`
	OutputTokens          int64     `gorm:"column:output_tokens"`
	ReasoningTokens       int64     `gorm:"column:reasoning_tokens"`
	TotalTokens           int64     `gorm:"column:total_tokens"`
	NetworkIngressBytes   int64     `gorm:"column:network_ingress_bytes"`
	NetworkEgressBytes    int64     `gorm:"column:network_egress_bytes"`
	DurationMillis        int64     `gorm:"column:duration_millis"`
	ProviderCostMicros    int64     `gorm:"column:provider_cost_micros"`
	ProviderCostReported  bool      `gorm:"column:provider_cost_reported;not null;default:false"`
	CurrencyCode          string    `gorm:"column:currency_code"`
	Final                 bool      `gorm:"column:final"`
	LatestEventSequence   int64     `gorm:"column:latest_event_sequence"`
	NetworkReportSequence int64     `gorm:"column:network_report_sequence"`
	CreatedAt             time.Time `gorm:"column:created_at"`
	UpdatedAt             time.Time `gorm:"column:updated_at"`
}

func (ExecutionUsageSummary) TableName() string { return "execution_usage_summaries" }
