package observability

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/databasetime"
)

type executionQueueGroup struct {
	TargetKind    string  `gorm:"column:target_kind"`
	CapacityClass *string `gorm:"column:capacity_class"`
	QueueClass    string  `gorm:"column:queue_class"`
	Depth         int64   `gorm:"column:depth"`
	OldestQueued  string  `gorm:"column:oldest_queued_at"`
}

type executionQueueMetricKey struct {
	TargetKind    string
	CapacityClass string
	QueueClass    string
}

type executionQueueMetric struct {
	Depth        int64
	OldestQueued time.Time
}

func (r *Registry) writeExecutionQueueMetrics(
	ctx context.Context,
	output *bytes.Buffer,
	now time.Time,
) error {
	var rows []executionQueueGroup
	if err := r.db.WithContext(ctx).Table("agent_executions").
		Select(`target_kind, capacity_class, queue_class, COUNT(*) AS depth,
			MIN(queued_at) AS oldest_queued_at`).
		Where("status IN ?", []string{"queued", "recovering"}).
		Group("target_kind, capacity_class, queue_class").
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("collect durable Execution queue metrics: %w", err)
	}

	metrics := make(map[executionQueueMetricKey]executionQueueMetric, len(rows))
	for _, row := range rows {
		oldestQueued, err := databasetime.Parse(row.OldestQueued)
		if err != nil {
			return fmt.Errorf("collect durable Execution queue metrics: %w", err)
		}
		key := executionQueueMetricKey{
			TargetKind:    boundedTargetKind(strings.ToLower(strings.TrimSpace(row.TargetKind))),
			CapacityClass: boundedExecutionQueueCapacityClass(row.CapacityClass),
			QueueClass:    boundedExecutionQueueClass(row.QueueClass),
		}
		metric := metrics[key]
		metric.Depth += row.Depth
		if metric.OldestQueued.IsZero() || oldestQueued.Before(metric.OldestQueued) {
			metric.OldestQueued = oldestQueued
		}
		metrics[key] = metric
	}

	keys := make([]executionQueueMetricKey, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].TargetKind != keys[right].TargetKind {
			return keys[left].TargetKind < keys[right].TargetKind
		}
		if keys[left].CapacityClass != keys[right].CapacityClass {
			return keys[left].CapacityClass < keys[right].CapacityClass
		}
		return keys[left].QueueClass < keys[right].QueueClass
	})

	writeHelp(
		output,
		"synara_execution_queue_depth",
		"Authoritative queued or recovering Execution count by bounded target kind, capacity class, and queue class.",
		"gauge",
	)
	for _, key := range keys {
		metricLabels := labels(map[string]string{
			"target_kind":    key.TargetKind,
			"capacity_class": key.CapacityClass,
			"queue_class":    key.QueueClass,
		})
		fmt.Fprintf(output, "synara_execution_queue_depth%s %d\n", metricLabels, metrics[key].Depth)
	}
	writeHelp(
		output,
		"synara_execution_queue_oldest_age_seconds",
		"Age of the oldest queued or recovering Execution by bounded target kind, capacity class, and queue class.",
		"gauge",
	)
	for _, key := range keys {
		metric := metrics[key]
		metricLabels := labels(map[string]string{
			"target_kind":    key.TargetKind,
			"capacity_class": key.CapacityClass,
			"queue_class":    key.QueueClass,
		})
		oldestAgeSeconds := 0.0
		if !metric.OldestQueued.IsZero() && metric.OldestQueued.Before(now) {
			oldestAgeSeconds = now.Sub(metric.OldestQueued).Seconds()
		}
		fmt.Fprintf(
			output,
			"synara_execution_queue_oldest_age_seconds%s %s\n",
			metricLabels,
			formatFloat(oldestAgeSeconds),
		)
	}
	return nil
}

func boundedExecutionQueueClass(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "interactive", "automation", "batch":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func boundedExecutionQueueCapacityClass(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(*value)) {
	case "standard":
		return "standard"
	case "interactive":
		return "interactive"
	default:
		return "other"
	}
}
