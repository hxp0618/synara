package persistence

import "time"

type OutboxPressureState struct {
	SingletonKey       string        `gorm:"column:singleton_key;primaryKey"`
	BaseBatchSize      int           `gorm:"column:base_batch_size;not null"`
	MaxBatchSize       int           `gorm:"column:max_batch_size;not null"`
	MaxConcurrency     int           `gorm:"column:max_concurrency;not null"`
	ScaleUpDepth       int64         `gorm:"column:scale_up_depth;not null"`
	TargetDelay        time.Duration `gorm:"column:target_delay_nanoseconds;not null"`
	ThrottleDepth      int64         `gorm:"column:throttle_depth;not null"`
	Pending            int64         `gorm:"column:pending;not null"`
	OldestPending      time.Duration `gorm:"column:oldest_pending_nanoseconds;not null"`
	DesiredBatchSize   int           `gorm:"column:desired_batch_size;not null"`
	DesiredConcurrency int           `gorm:"column:desired_concurrency;not null"`
	Status             string        `gorm:"column:status;not null"`
	ObservedAt         time.Time     `gorm:"column:observed_at;not null"`
	Version            int64         `gorm:"column:version;not null;default:1"`
	UpdatedAt          time.Time     `gorm:"column:updated_at;not null"`
}

func (OutboxPressureState) TableName() string { return "outbox_pressure_state" }
