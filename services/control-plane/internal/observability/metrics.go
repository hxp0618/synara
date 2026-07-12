package observability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

var requestDurationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type requestKey struct {
	method string
	route  string
	status int
}

type requestMetric struct {
	count   uint64
	sum     float64
	buckets [len(requestDurationBuckets)]uint64
}

type backgroundMetric struct {
	runs            uint64
	failures        uint64
	durationSeconds float64
	lastSuccess     time.Time
}

// Registry contains only process-local monotonic telemetry. Authoritative domain
// state is read from PostgreSQL/SQLite when Prometheus scrapes the endpoint, so
// Worker and Execution counts cannot drift from the database.
type Registry struct {
	db *gorm.DB

	mu         sync.RWMutex
	requests   map[requestKey]requestMetric
	background map[string]backgroundMetric
}

func New(db *gorm.DB) *Registry {
	return &Registry{
		db: db, requests: make(map[requestKey]requestMetric),
		background: make(map[string]backgroundMetric),
	}
}

func (r *Registry) ObserveHTTP(method, route string, status int, duration time.Duration) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "UNKNOWN"
	}
	route = normalizeRoute(method, route)
	key := requestKey{method: method, route: route, status: status}
	seconds := duration.Seconds()
	r.mu.Lock()
	metric := r.requests[key]
	metric.count++
	metric.sum += seconds
	for index, upperBound := range requestDurationBuckets {
		if seconds <= upperBound {
			metric.buckets[index]++
		}
	}
	r.requests[key] = metric
	r.mu.Unlock()
}

func (r *Registry) ObserveBackground(kind string, started time.Time, err error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "docker", "kubernetes", "retention":
	default:
		kind = "other"
	}
	r.mu.Lock()
	metric := r.background[kind]
	metric.runs++
	metric.durationSeconds += time.Since(started).Seconds()
	if err != nil {
		metric.failures++
	} else {
		metric.lastSuccess = time.Now().UTC()
	}
	r.background[kind] = metric
	r.mu.Unlock()
}

func (r *Registry) Gather(ctx context.Context) ([]byte, error) {
	var output bytes.Buffer
	r.writeProcessMetrics(&output)
	if err := r.writeDatabaseMetrics(ctx, &output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r *Registry) writeProcessMetrics(output *bytes.Buffer) {
	r.mu.RLock()
	requests := make(map[requestKey]requestMetric, len(r.requests))
	for key, value := range r.requests {
		requests[key] = value
	}
	background := make(map[string]backgroundMetric, len(r.background))
	for key, value := range r.background {
		background[key] = value
	}
	r.mu.RUnlock()

	requestKeys := make([]requestKey, 0, len(requests))
	for key := range requests {
		requestKeys = append(requestKeys, key)
	}
	sort.Slice(requestKeys, func(i, j int) bool {
		if requestKeys[i].route != requestKeys[j].route {
			return requestKeys[i].route < requestKeys[j].route
		}
		if requestKeys[i].method != requestKeys[j].method {
			return requestKeys[i].method < requestKeys[j].method
		}
		return requestKeys[i].status < requestKeys[j].status
	})
	writeHelp(output, "synara_http_requests_total", "Control-plane HTTP requests by bounded route pattern, method, and status.", "counter")
	writeHelp(output, "synara_http_request_duration_seconds", "Control-plane HTTP request latency by bounded route pattern, method, and status.", "histogram")
	for _, key := range requestKeys {
		metric := requests[key]
		labels := requestLabels(key)
		fmt.Fprintf(output, "synara_http_requests_total%s %d\n", labels, metric.count)
		for index, upperBound := range requestDurationBuckets {
			fmt.Fprintf(output, "synara_http_request_duration_seconds_bucket%s %d\n", addLabel(labels, "le", strconv.FormatFloat(upperBound, 'g', -1, 64)), metric.buckets[index])
		}
		fmt.Fprintf(output, "synara_http_request_duration_seconds_bucket%s %d\n", addLabel(labels, "le", "+Inf"), metric.count)
		fmt.Fprintf(output, "synara_http_request_duration_seconds_sum%s %s\n", labels, formatFloat(metric.sum))
		fmt.Fprintf(output, "synara_http_request_duration_seconds_count%s %d\n", labels, metric.count)
	}

	writeHelp(output, "synara_background_runs_total", "Control-plane background reconciliation and retention runs.", "counter")
	writeHelp(output, "synara_background_failures_total", "Failed control-plane background reconciliation and retention runs.", "counter")
	writeHelp(output, "synara_background_duration_seconds_total", "Cumulative duration of background reconciliation and retention runs.", "counter")
	writeHelp(output, "synara_background_last_success_unixtime", "Unix timestamp of the latest successful background run.", "gauge")
	kinds := make([]string, 0, len(background))
	for kind := range background {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		metric := background[kind]
		labels := labels(map[string]string{"kind": kind})
		fmt.Fprintf(output, "synara_background_runs_total%s %d\n", labels, metric.runs)
		fmt.Fprintf(output, "synara_background_failures_total%s %d\n", labels, metric.failures)
		fmt.Fprintf(output, "synara_background_duration_seconds_total%s %s\n", labels, formatFloat(metric.durationSeconds))
		lastSuccess := int64(0)
		if !metric.lastSuccess.IsZero() {
			lastSuccess = metric.lastSuccess.Unix()
		}
		fmt.Fprintf(output, "synara_background_last_success_unixtime%s %d\n", labels, lastSuccess)
	}
}

type groupedCount struct {
	Status     string `gorm:"column:status"`
	TargetKind string `gorm:"column:target_kind"`
	Count      int64  `gorm:"column:count"`
}

func (r *Registry) writeDatabaseMetrics(ctx context.Context, output *bytes.Buffer) error {
	executions, err := groupedCounts(ctx, r.db, "agent_executions", "status", "target_kind")
	if err != nil {
		return fmt.Errorf("collect execution metrics: %w", err)
	}
	workers, err := groupedCounts(ctx, r.db, "worker_instances", "status", "target_kind")
	if err != nil {
		return fmt.Errorf("collect worker metrics: %w", err)
	}
	targets, err := groupedCounts(ctx, r.db, "execution_targets", "status", "kind AS target_kind")
	if err != nil {
		return fmt.Errorf("collect execution target metrics: %w", err)
	}
	now := time.Now().UTC()
	var activeLeases, expiredLeases, outboxPending, outboxRetrying, outboxDeadLetter int64
	if err := r.db.WithContext(ctx).Table("worker_leases").Where("expires_at > ?", now).Count(&activeLeases).Error; err != nil {
		return fmt.Errorf("collect active lease metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("worker_leases").Where("expires_at <= ?", now).Count(&expiredLeases).Error; err != nil {
		return fmt.Errorf("collect expired lease metrics: %w", err)
	}
	outboxPendingQuery := r.db.WithContext(ctx).Table("outbox_messages").Where("published_at IS NULL AND dead_lettered_at IS NULL")
	if err := outboxPendingQuery.Count(&outboxPending).Error; err != nil {
		return fmt.Errorf("collect outbox metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("outbox_messages").
		Where("published_at IS NULL AND dead_lettered_at IS NULL AND attempts > 0").Count(&outboxRetrying).Error; err != nil {
		return fmt.Errorf("collect retrying outbox metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("outbox_messages").Where("dead_lettered_at IS NOT NULL").Count(&outboxDeadLetter).Error; err != nil {
		return fmt.Errorf("collect dead-letter outbox metrics: %w", err)
	}
	var oldestOutbox persistence.OutboxMessage
	oldestOutboxErr := r.db.WithContext(ctx).Select("created_at").
		Where("published_at IS NULL AND dead_lettered_at IS NULL").Order("created_at, id").Take(&oldestOutbox).Error
	if oldestOutboxErr != nil && !errors.Is(oldestOutboxErr, gorm.ErrRecordNotFound) {
		return fmt.Errorf("collect oldest outbox metrics: %w", oldestOutboxErr)
	}
	oldestOutboxSeconds := 0.0
	if oldestOutboxErr == nil && oldestOutbox.CreatedAt.Before(now) {
		oldestOutboxSeconds = now.Sub(oldestOutbox.CreatedAt).Seconds()
	}

	writeGroupedGauge(output, "synara_executions", "Authoritative Execution count by status and target kind.", executions)
	writeGroupedGauge(output, "synara_workers", "Authoritative Worker count by status and target kind.", workers)
	writeGroupedGauge(output, "synara_execution_targets", "Authoritative Execution Target count by status and kind.", targets)
	writeHelp(output, "synara_worker_leases", "Authoritative Worker Lease count by expiration state.", "gauge")
	fmt.Fprintf(output, "synara_worker_leases%s %d\n", labels(map[string]string{"state": "active"}), activeLeases)
	fmt.Fprintf(output, "synara_worker_leases%s %d\n", labels(map[string]string{"state": "expired"}), expiredLeases)
	writeHelp(output, "synara_outbox_pending", "Unpublished authoritative outbox message count.", "gauge")
	fmt.Fprintf(output, "synara_outbox_pending %d\n", outboxPending)
	writeHelp(output, "synara_outbox_retrying", "Outbox messages waiting for another publish attempt.", "gauge")
	fmt.Fprintf(output, "synara_outbox_retrying %d\n", outboxRetrying)
	writeHelp(output, "synara_outbox_dead_letter", "Outbox messages that exhausted publish attempts.", "gauge")
	fmt.Fprintf(output, "synara_outbox_dead_letter %d\n", outboxDeadLetter)
	writeHelp(output, "synara_outbox_oldest_pending_seconds", "Age of the oldest pending outbox message.", "gauge")
	fmt.Fprintf(output, "synara_outbox_oldest_pending_seconds %s\n", formatFloat(oldestOutboxSeconds))
	writeHelp(output, "synara_metrics_collection_success", "Whether authoritative database metrics were collected successfully.", "gauge")
	output.WriteString("synara_metrics_collection_success 1\n")
	return nil
}

func groupedCounts(ctx context.Context, db *gorm.DB, table, statusColumn, kindExpression string) ([]groupedCount, error) {
	var rows []groupedCount
	err := db.WithContext(ctx).Table(table).
		Select(statusColumn + " AS status, " + kindExpression + ", COUNT(*) AS count").
		Group(statusColumn + ", target_kind").Order(statusColumn + ", target_kind").Scan(&rows).Error
	return rows, err
}

func writeGroupedGauge(output *bytes.Buffer, name, help string, rows []groupedCount) {
	writeHelp(output, name, help, "gauge")
	for _, row := range rows {
		fmt.Fprintf(output, "%s%s %d\n", name, labels(map[string]string{"status": row.Status, "target_kind": row.TargetKind}), row.Count)
	}
}

func normalizeRoute(method, pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "unmatched"
	}
	prefix := method + " "
	if strings.HasPrefix(pattern, prefix) {
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, prefix))
	}
	if !strings.HasPrefix(pattern, "/") || len(pattern) > 240 {
		return "unmatched"
	}
	for _, segment := range strings.Split(pattern, "/") {
		if _, err := uuid.Parse(segment); err == nil {
			return "unmatched"
		}
	}
	return pattern
}

func requestLabels(key requestKey) string {
	return labels(map[string]string{
		"method": key.method, "route": key.route, "status": strconv.Itoa(key.status),
	})
}

func labels(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"=\""+escapeLabel(values[key])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func addLabel(existing, key, value string) string {
	return strings.TrimSuffix(existing, "}") + "," + key + "=\"" + escapeLabel(value) + "\"}"
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return strings.ReplaceAll(value, "\"", "\\\"")
}

func writeHelp(output *bytes.Buffer, name, help, metricType string) {
	fmt.Fprintf(output, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
