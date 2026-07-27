package persistence

import (
	"time"

	"github.com/google/uuid"
)

type WorkerPoolAutoscalingPolicy struct {
	WorkerPoolID                   uuid.UUID `gorm:"column:worker_pool_id;type:uuid;primaryKey"`
	WorkerPoolVersion              int64     `gorm:"column:worker_pool_version;primaryKey"`
	TenantID                       uuid.UUID `gorm:"column:tenant_id;type:uuid;not null"`
	ExecutionTargetID              uuid.UUID `gorm:"column:execution_target_id;type:uuid;not null"`
	Enabled                        bool      `gorm:"column:enabled;not null;default:false"`
	MinIdleUnits                   int       `gorm:"column:min_idle_units;not null;default:0"`
	MaxIdleUnits                   int       `gorm:"column:max_idle_units;not null;default:0"`
	TargetQueueDelaySeconds        int       `gorm:"column:target_queue_delay_seconds;not null;default:30"`
	InteractiveColdStartMaxSeconds *int      `gorm:"column:interactive_cold_start_max_seconds"`
	ScaleUpStep                    int       `gorm:"column:scale_up_step;not null;default:1"`
	ScaleDownStep                  int       `gorm:"column:scale_down_step;not null;default:1"`
	CooldownSeconds                int       `gorm:"column:cooldown_seconds;not null;default:30"`
	ScaleDownStabilizationSeconds  int       `gorm:"column:scale_down_stabilization_seconds;not null;default:300"`
	Version                        int64     `gorm:"column:version;not null;default:1"`
	UpdatedBy                      uuid.UUID `gorm:"column:updated_by;type:uuid;not null"`
	CreatedAt                      time.Time `gorm:"column:created_at;not null"`
	UpdatedAt                      time.Time `gorm:"column:updated_at;not null"`
}

func (WorkerPoolAutoscalingPolicy) TableName() string { return "worker_pool_autoscaling_policies" }

type WorkerPoolAutoscalingState struct {
	WorkerPoolID        uuid.UUID  `gorm:"column:worker_pool_id;type:uuid;primaryKey"`
	WorkerPoolVersion   int64      `gorm:"column:worker_pool_version;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id;type:uuid;not null"`
	ExecutionTargetID   uuid.UUID  `gorm:"column:execution_target_id;type:uuid;not null"`
	PolicyVersion       int64      `gorm:"column:policy_version;not null"`
	DesiredIdleUnits    int        `gorm:"column:desired_idle_units;not null"`
	QueueDepth          int64      `gorm:"column:queue_depth;not null"`
	OldestQueuedAt      *time.Time `gorm:"column:oldest_queued_at"`
	ReadyIdleUnits      int        `gorm:"column:ready_idle_units;not null"`
	DecisionReason      string     `gorm:"column:decision_reason;not null"`
	ColdStartGateStatus string     `gorm:"column:cold_start_gate_status;not null;default:unknown"`
	LastQueueActiveAt   *time.Time `gorm:"column:last_queue_active_at"`
	LastScaledAt        *time.Time `gorm:"column:last_scaled_at"`
	DecisionVersion     int64      `gorm:"column:decision_version;not null;default:1"`
	ObservedAt          time.Time  `gorm:"column:observed_at;not null"`
	UpdatedAt           time.Time  `gorm:"column:updated_at;not null"`
}

func (WorkerPoolAutoscalingState) TableName() string { return "worker_pool_autoscaling_state" }
