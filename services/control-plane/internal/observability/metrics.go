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

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

var requestDurationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
var executionStartupBuckets = [...]float64{1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600, 1800}

type requestKey struct {
	method string
	route  string
	status int
}

type durationMetric struct {
	count   uint64
	sum     float64
	buckets [len(requestDurationBuckets)]uint64
}

type startupDurationMetric struct {
	count   uint64
	sum     float64
	buckets [len(executionStartupBuckets)]uint64
}

type requestMetric = durationMetric

type loginMetricKey struct {
	method string
	result string
}

type backgroundMetric struct {
	runs            uint64
	failures        uint64
	durationSeconds float64
	lastSuccess     time.Time
}

type artifactMetricKey struct {
	operation string
	result    string
}

type artifactMetric struct {
	count uint64
	bytes uint64
}

type Config struct {
	SessionIdleTTL         time.Duration
	WorkerHeartbeatTimeout time.Duration
}

type sseCatchupMetric struct {
	count    uint64
	failures uint64
	events   uint64
	sum      float64
	buckets  [len(requestDurationBuckets)]uint64
}

// Registry contains only process-local monotonic telemetry. Authoritative domain
// state is read from PostgreSQL/SQLite when Prometheus scrapes the endpoint, so
// Worker and Execution counts cannot drift from the database.
type Registry struct {
	db                     *gorm.DB
	sessionIdleTTL         time.Duration
	workerHeartbeatTimeout time.Duration

	mu          sync.RWMutex
	requests    map[requestKey]requestMetric
	logins      map[loginMetricKey]uint64
	leaseRenew  map[string]uint64
	fencing     map[string]uint64
	eventAppend map[string]durationMetric
	background  map[string]backgroundMetric
	artifacts   map[artifactMetricKey]artifactMetric
	sseCatchup  sseCatchupMetric
	sseLimits   map[string]uint64
}

func New(db *gorm.DB, configurations ...Config) *Registry {
	sessionIdleTTL := 7 * 24 * time.Hour
	workerHeartbeatTimeout := 90 * time.Second
	if len(configurations) > 0 {
		if configurations[0].SessionIdleTTL > 0 {
			sessionIdleTTL = configurations[0].SessionIdleTTL
		}
		if configurations[0].WorkerHeartbeatTimeout > 0 {
			workerHeartbeatTimeout = configurations[0].WorkerHeartbeatTimeout
		}
	}
	return &Registry{
		db: db, sessionIdleTTL: sessionIdleTTL, workerHeartbeatTimeout: workerHeartbeatTimeout,
		requests: make(map[requestKey]requestMetric),
		logins:   make(map[loginMetricKey]uint64), leaseRenew: make(map[string]uint64),
		fencing: make(map[string]uint64), eventAppend: make(map[string]durationMetric),
		background: make(map[string]backgroundMetric), artifacts: make(map[artifactMetricKey]artifactMetric),
		sseLimits: make(map[string]uint64),
	}
}

func (r *Registry) ObserveHTTP(method, route string, status int, duration time.Duration, problemCode string) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "UNKNOWN"
	}
	route = normalizeRoute(method, route)
	key := requestKey{method: method, route: route, status: status}
	seconds := duration.Seconds()
	problemCode = strings.TrimSpace(problemCode)
	r.mu.Lock()
	metric := r.requests[key]
	observeDuration(&metric, seconds)
	r.requests[key] = metric
	r.observeLifecycleRequestLocked(method, route, status, seconds, problemCode)
	r.mu.Unlock()
}

func (r *Registry) observeLifecycleRequestLocked(method, route string, status int, seconds float64, problemCode string) {
	if loginMethod, ok := loginMethodForRequest(method, route); ok {
		result := "failure"
		if status >= 200 && status < 400 {
			result = "success"
		}
		r.logins[loginMetricKey{method: loginMethod, result: result}]++
	}

	if route == "/v1/workers/executions/{executionID}/renew" {
		r.leaseRenew[requestResult(status)]++
	}
	if route == "/v1/workers/executions/{executionID}/events" {
		result := requestResult(status)
		metric := r.eventAppend[result]
		observeDuration(&metric, seconds)
		r.eventAppend[result] = metric
	}
	if strings.HasPrefix(route, "/v1/workers/") && isWorkerFencingProblem(problemCode) {
		r.fencing[fencingOperation(route)]++
	}
}

func observeDuration(metric *durationMetric, seconds float64) {
	metric.count++
	metric.sum += seconds
	for index, upperBound := range requestDurationBuckets {
		if seconds <= upperBound {
			metric.buckets[index]++
		}
	}
}

func loginMethodForRequest(method, route string) (string, bool) {
	switch route {
	case "/v1/auth/dev-login":
		return "dev", true
	case "/v1/auth/sso/{connectionID}/callback":
		if method == "POST" {
			return "saml", true
		}
		if method == "GET" {
			return "oidc", true
		}
	}
	return "", false
}

func requestResult(status int) string {
	switch {
	case status >= 200 && status < 400:
		return "success"
	case status >= 400 && status < 500:
		return "rejected"
	default:
		return "failure"
	}
}

func isWorkerFencingProblem(code string) bool {
	if strings.HasSuffix(code, "_fenced") {
		return true
	}
	switch code {
	case "invalid_lease_token", "lease_expired", "lease_not_current", "interaction_lease_expired", "worker_token_revoked":
		return true
	default:
		return false
	}
}

func fencingOperation(route string) string {
	switch {
	case route == "/v1/workers/heartbeat":
		return "heartbeat"
	case route == "/v1/workers/executions/{executionID}/renew":
		return "lease-renew"
	case route == "/v1/workers/executions/{executionID}/events":
		return "session-event"
	case strings.Contains(route, "/workspace-cleanups/"):
		return "workspace-cleanup"
	case strings.Contains(route, "/interaction-resolutions/"):
		return "interaction"
	case strings.Contains(route, "/control-commands/"):
		return "control-command"
	case strings.Contains(route, "/artifacts"):
		return "artifact"
	case strings.HasPrefix(route, "/v1/workers/executions/"):
		return "execution"
	default:
		return "other"
	}
}

func (r *Registry) ObserveBackground(kind string, started time.Time, err error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "docker", "kubernetes", "target-failover", "resource-lifecycle",
		"worker-release-auto-rollback", "retention", "billing-import-scheduler",
		"billing-shared-allocation-scheduler", "outbox":
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

func (r *Registry) ObserveArtifact(operation string, bytes int64, err error) {
	operation = strings.ToLower(strings.TrimSpace(operation))
	switch operation {
	case "create", "complete", "delete", "cleanup":
	default:
		operation = "other"
	}
	result := "success"
	if err != nil {
		result = "failure"
	}
	key := artifactMetricKey{operation: operation, result: result}
	r.mu.Lock()
	metric := r.artifacts[key]
	metric.count++
	if bytes > 0 {
		metric.bytes += uint64(bytes)
	}
	r.artifacts[key] = metric
	r.mu.Unlock()
}

func (r *Registry) ObserveSSECatchup(duration time.Duration, events int, err error) {
	seconds := duration.Seconds()
	r.mu.Lock()
	r.sseCatchup.count++
	r.sseCatchup.sum += seconds
	if events > 0 {
		r.sseCatchup.events += uint64(events)
	}
	if err != nil {
		r.sseCatchup.failures++
	}
	for index, upperBound := range requestDurationBuckets {
		if seconds <= upperBound {
			r.sseCatchup.buckets[index]++
		}
	}
	r.mu.Unlock()
}

func (r *Registry) ObserveSSELimit(scope string) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != "user" && scope != "tenant" {
		scope = "other"
	}
	r.mu.Lock()
	r.sseLimits[scope]++
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
	requests := cloneMap(r.requests)
	logins := cloneMap(r.logins)
	leaseRenew := cloneMap(r.leaseRenew)
	fencing := cloneMap(r.fencing)
	eventAppend := cloneMap(r.eventAppend)
	background := cloneMap(r.background)
	artifacts := cloneMap(r.artifacts)
	sseCatchup := r.sseCatchup
	sseLimits := cloneMap(r.sseLimits)
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

	loginKeys := make([]loginMetricKey, 0, len(logins))
	for key := range logins {
		loginKeys = append(loginKeys, key)
	}
	sort.Slice(loginKeys, func(i, j int) bool {
		if loginKeys[i].method != loginKeys[j].method {
			return loginKeys[i].method < loginKeys[j].method
		}
		return loginKeys[i].result < loginKeys[j].result
	})
	writeHelp(output, "synara_login_attempts_total", "Completed control-plane login attempts by bounded authentication method and result.", "counter")
	for _, key := range loginKeys {
		fmt.Fprintf(output, "synara_login_attempts_total%s %d\n", labels(map[string]string{"method": key.method, "result": key.result}), logins[key])
	}

	writeHelp(output, "synara_worker_lease_renewals_total", "Worker execution Lease renewal requests by bounded result.", "counter")
	leaseResults := sortedStringKeys(leaseRenew)
	for _, result := range leaseResults {
		fmt.Fprintf(output, "synara_worker_lease_renewals_total%s %d\n", labels(map[string]string{"result": result}), leaseRenew[result])
	}

	writeHelp(output, "synara_worker_fencing_rejections_total", "Worker requests rejected by Lease, Generation, or Worker incarnation fencing.", "counter")
	fencingOperations := sortedStringKeys(fencing)
	for _, operation := range fencingOperations {
		fmt.Fprintf(output, "synara_worker_fencing_rejections_total%s %d\n", labels(map[string]string{"operation": operation}), fencing[operation])
	}

	writeHelp(output, "synara_session_event_append_duration_seconds", "Worker Runtime Event append latency by bounded result.", "histogram")
	eventResults := sortedStringKeys(eventAppend)
	for _, result := range eventResults {
		metric := eventAppend[result]
		resultLabels := labels(map[string]string{"result": result})
		for index, upperBound := range requestDurationBuckets {
			fmt.Fprintf(output, "synara_session_event_append_duration_seconds_bucket%s %d\n", addLabel(resultLabels, "le", strconv.FormatFloat(upperBound, 'g', -1, 64)), metric.buckets[index])
		}
		fmt.Fprintf(output, "synara_session_event_append_duration_seconds_bucket%s %d\n", addLabel(resultLabels, "le", "+Inf"), metric.count)
		fmt.Fprintf(output, "synara_session_event_append_duration_seconds_sum%s %s\n", resultLabels, formatFloat(metric.sum))
		fmt.Fprintf(output, "synara_session_event_append_duration_seconds_count%s %d\n", resultLabels, metric.count)
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

	artifactKeys := make([]artifactMetricKey, 0, len(artifacts))
	for key := range artifacts {
		artifactKeys = append(artifactKeys, key)
	}
	sort.Slice(artifactKeys, func(i, j int) bool {
		if artifactKeys[i].operation != artifactKeys[j].operation {
			return artifactKeys[i].operation < artifactKeys[j].operation
		}
		return artifactKeys[i].result < artifactKeys[j].result
	})
	writeHelp(output, "synara_artifact_operations_total", "Artifact lifecycle operations by bounded operation and result.", "counter")
	writeHelp(output, "synara_artifact_bytes_total", "Artifact bytes processed by bounded operation and result.", "counter")
	for _, key := range artifactKeys {
		metric := artifacts[key]
		metricLabels := labels(map[string]string{"operation": key.operation, "result": key.result})
		fmt.Fprintf(output, "synara_artifact_operations_total%s %d\n", metricLabels, metric.count)
		fmt.Fprintf(output, "synara_artifact_bytes_total%s %d\n", metricLabels, metric.bytes)
	}

	writeHelp(output, "synara_sse_catchup_duration_seconds", "Database-backed SSE backlog catch-up duration.", "histogram")
	for index, upperBound := range requestDurationBuckets {
		fmt.Fprintf(output, "synara_sse_catchup_duration_seconds_bucket%s %d\n", labels(map[string]string{"le": strconv.FormatFloat(upperBound, 'g', -1, 64)}), sseCatchup.buckets[index])
	}
	fmt.Fprintf(output, "synara_sse_catchup_duration_seconds_bucket%s %d\n", labels(map[string]string{"le": "+Inf"}), sseCatchup.count)
	fmt.Fprintf(output, "synara_sse_catchup_duration_seconds_sum %s\n", formatFloat(sseCatchup.sum))
	fmt.Fprintf(output, "synara_sse_catchup_duration_seconds_count %d\n", sseCatchup.count)
	writeHelp(output, "synara_sse_catchup_failures_total", "Failed database-backed SSE catch-up attempts.", "counter")
	fmt.Fprintf(output, "synara_sse_catchup_failures_total %d\n", sseCatchup.failures)
	writeHelp(output, "synara_sse_catchup_events_total", "Session Events delivered through database-backed SSE catch-up.", "counter")
	fmt.Fprintf(output, "synara_sse_catchup_events_total %d\n", sseCatchup.events)
	writeHelp(output, "synara_sse_connection_rejections_total", "Rejected SSE connections by bounded limit scope.", "counter")
	limitScopes := make([]string, 0, len(sseLimits))
	for scope := range sseLimits {
		limitScopes = append(limitScopes, scope)
	}
	sort.Strings(limitScopes)
	for _, scope := range limitScopes {
		fmt.Fprintf(output, "synara_sse_connection_rejections_total%s %d\n", labels(map[string]string{"scope": scope}), sseLimits[scope])
	}
}

type groupedCount struct {
	Status     string `gorm:"column:status"`
	TargetKind string `gorm:"column:target_kind"`
	Count      int64  `gorm:"column:count"`
}

type valueCount struct {
	Value string `gorm:"column:value"`
	Count int64  `gorm:"column:count"`
}

type pairedValueCount struct {
	First  string `gorm:"column:first"`
	Second string `gorm:"column:second"`
	Count  int64  `gorm:"column:count"`
}

type providerCredentialAccessLeaseCount struct {
	State string `gorm:"column:access_state"`
	Count int64  `gorm:"column:count"`
}

type executionStartupSample struct {
	RecoveryReason  string     `gorm:"column:recovery_reason"`
	TargetKind      string     `gorm:"column:target_kind"`
	ExecutionID     uuid.UUID  `gorm:"column:execution_id"`
	Generation      int64      `gorm:"column:generation"`
	BundleCreatedAt time.Time  `gorm:"column:bundle_created_at"`
	LeasedAt        *time.Time `gorm:"column:leased_at"`
	StartedAt       *time.Time `gorm:"column:started_at"`
	ProviderReadyAt *time.Time `gorm:"column:provider_ready_at"`
	TerminalAt      *time.Time `gorm:"column:terminal_at"`
}

type providerResumeClaimDecisionMetricKey struct {
	TargetKind       string
	SelectedStrategy string
	ReasonCode       string
}

type providerResumeRuntimeFallbackMetricKey struct {
	Provider   string
	ReasonCode string
}

func (r *Registry) writeDatabaseMetrics(ctx context.Context, output *bytes.Buffer) error {
	now := time.Now().UTC()
	executions, err := groupedCounts(ctx, r.db, "agent_executions", "status", "target_kind")
	if err != nil {
		return fmt.Errorf("collect execution metrics: %w", err)
	}
	if err := r.writeExecutionQueueMetrics(ctx, output, now); err != nil {
		return err
	}
	workers, err := groupedCounts(ctx, r.db, "worker_instances", "status", "target_kind")
	if err != nil {
		return fmt.Errorf("collect worker metrics: %w", err)
	}
	workerAdministrative, err := groupedCounts(ctx, r.db, "worker_instances", "administrative_status", "target_kind")
	if err != nil {
		return fmt.Errorf("collect Worker administrative metrics: %w", err)
	}
	staleWorkers, err := groupedCountsWhere(
		ctx, r.db, "worker_instances", "status", "target_kind",
		"administrative_status <> ? AND status IN ? AND last_heartbeat_at <= ?",
		"revoked", []string{"online", "draining"}, now.Add(-r.workerHeartbeatTimeout),
	)
	if err != nil {
		return fmt.Errorf("collect stale worker metrics: %w", err)
	}
	targets, err := groupedCounts(ctx, r.db, "execution_targets", "status", "kind AS target_kind")
	if err != nil {
		return fmt.Errorf("collect execution target metrics: %w", err)
	}
	sessionResourceStates, err := pairedCountsWhere(
		ctx, r.db, "agent_sessions", "status", "resource_state",
		"status IN ?", []string{"active", "suspended"},
	)
	if err != nil {
		return fmt.Errorf("collect session resource state metrics: %w", err)
	}
	sessionWarmPoolModes, err := pairedCountsWhere(
		ctx, r.db, "agent_sessions", "status", "warm_pool_mode",
		"status IN ?", []string{"active", "suspended"},
	)
	if err != nil {
		return fmt.Errorf("collect session warm pool mode metrics: %w", err)
	}
	resourceSuspendAttempts, err := pairedCountsWhere(
		ctx, r.db, "execution_suspend_attempts", "status", "reason", "",
	)
	if err != nil {
		return fmt.Errorf("collect resource suspend attempt metrics: %w", err)
	}
	resourceSuspendCompletionModes, err := pairedCountsWhere(
		ctx, r.db, "execution_suspend_attempts", "status", "completion_mode", "",
	)
	if err != nil {
		return fmt.Errorf("collect resource suspend completion mode metrics: %w", err)
	}
	recoveryBundles, err := valueCountsWhere(
		ctx, r.db, "execution_recovery_bundles", "recovery_reason", "",
	)
	if err != nil {
		return fmt.Errorf("collect recovery bundle metrics: %w", err)
	}
	startupSamples, err := r.executionStartupSamples(ctx)
	if err != nil {
		return fmt.Errorf("collect execution startup metrics: %w", err)
	}
	claimResumeDecisions, runtimeResumeFallbacks, err := r.providerResumeMetrics(ctx)
	if err != nil {
		return fmt.Errorf("collect Provider resume metrics: %w", err)
	}
	providerCredentialAccessLeases, err := r.providerCredentialAccessLeaseCounts(ctx, now)
	if err != nil {
		return fmt.Errorf("collect Provider Credential access Lease metrics: %w", err)
	}
	var activeLeases, expiredLeases, outboxPending, outboxRetrying, outboxDeadLetter int64
	var activeSSEConnections, expiredSSEConnections, activeLoginSessions, readyArtifactBytes int64
	if err := r.db.WithContext(ctx).Table("worker_leases").Where("expires_at > ?", now).Count(&activeLeases).Error; err != nil {
		return fmt.Errorf("collect active lease metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("worker_leases").Where("expires_at <= ?", now).Count(&expiredLeases).Error; err != nil {
		return fmt.Errorf("collect expired lease metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("sse_connection_leases").Where("expires_at > ?", now).Count(&activeSSEConnections).Error; err != nil {
		return fmt.Errorf("collect active SSE connection metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("sse_connection_leases").Where("expires_at <= ?", now).Count(&expiredSSEConnections).Error; err != nil {
		return fmt.Errorf("collect expired SSE connection metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("login_sessions").
		Where("revoked_at IS NULL AND expires_at > ? AND last_seen_at > ?", now, now.Add(-r.sessionIdleTTL)).Count(&activeLoginSessions).Error; err != nil {
		return fmt.Errorf("collect active login session metrics: %w", err)
	}
	if err := r.db.WithContext(ctx).Table("artifacts").Select("COALESCE(SUM(size_bytes), 0)").
		Where("status = ? AND deleted_at IS NULL", "ready").Scan(&readyArtifactBytes).Error; err != nil {
		return fmt.Errorf("collect ready Artifact byte metrics: %w", err)
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
	writeGroupedGauge(output, "synara_workers_administrative", "Authoritative Worker count by administrative status and target kind.", workerAdministrative)
	writeGroupedGauge(output, "synara_stale_workers", "Authoritative online or draining Worker count past the configured heartbeat timeout.", staleWorkers)
	writeGroupedGauge(output, "synara_execution_targets", "Authoritative Execution Target count by status and kind.", targets)
	writePairedGauge(
		output,
		"synara_sessions_resource_state",
		"Authoritative active or suspended Session count by operational status and Stage 4 resource state.",
		"session_status",
		"resource_state",
		sessionResourceStates,
	)
	writePairedGauge(
		output,
		"synara_sessions_warm_pool_mode",
		"Authoritative active or suspended Session inventory by operational status and configured warm pool mode; this is requested mode inventory, not prewarmed capacity.",
		"session_status",
		"warm_pool_mode",
		sessionWarmPoolModes,
	)
	writePairedGauge(
		output,
		"synara_resource_suspend_attempts",
		"Authoritative resource suspension attempt count by durable status and reason.",
		"status",
		"reason",
		resourceSuspendAttempts,
	)
	writePairedGauge(
		output,
		"synara_resource_suspend_completion_modes",
		"Authoritative resource suspension attempt count by durable status and completion proof mode.",
		"status",
		"completion_mode",
		resourceSuspendCompletionModes,
	)
	writeSingleLabelGauge(
		output,
		"synara_execution_recovery_bundles",
		"Authoritative immutable Recovery Bundle count by recovery reason. suspend-resume rows mark claimed resume generations.",
		"recovery_reason",
		recoveryBundles,
	)
	writeHelp(output, "synara_worker_leases", "Authoritative Worker Lease count by expiration state.", "gauge")
	fmt.Fprintf(output, "synara_worker_leases%s %d\n", labels(map[string]string{"state": "active"}), activeLeases)
	fmt.Fprintf(output, "synara_worker_leases%s %d\n", labels(map[string]string{"state": "expired"}), expiredLeases)
	writeHelp(
		output,
		"synara_provider_credential_access_leases",
		"Authoritative short-lived Provider Credential access Lease count by bounded semantic-renewal state.",
		"gauge",
	)
	providerCredentialAccessLeaseCountByState := make(map[string]int64, len(providerCredentialAccessLeases))
	for _, row := range providerCredentialAccessLeases {
		providerCredentialAccessLeaseCountByState[row.State] = row.Count
	}
	for _, state := range []string{"active", "refresh-window-closed", "expired", "credential-unavailable"} {
		fmt.Fprintf(
			output,
			"synara_provider_credential_access_leases%s %d\n",
			labels(map[string]string{"state": state}),
			providerCredentialAccessLeaseCountByState[state],
		)
	}
	writeHelp(output, "synara_sse_connections", "Authoritative SSE connection lease count by expiration state.", "gauge")
	fmt.Fprintf(output, "synara_sse_connections%s %d\n", labels(map[string]string{"state": "active"}), activeSSEConnections)
	fmt.Fprintf(output, "synara_sse_connections%s %d\n", labels(map[string]string{"state": "expired"}), expiredSSEConnections)
	writeHelp(output, "synara_active_login_sessions", "Authoritative active login session count.", "gauge")
	fmt.Fprintf(output, "synara_active_login_sessions %d\n", activeLoginSessions)
	writeHelp(output, "synara_artifact_ready_bytes", "Authoritative total bytes of ready non-deleted Artifacts.", "gauge")
	fmt.Fprintf(output, "synara_artifact_ready_bytes %d\n", readyArtifactBytes)
	if sqlDB, err := r.db.DB(); err == nil {
		stats := sqlDB.Stats()
		writeHelp(output, "synara_database_connections", "Database connection pool state.", "gauge")
		fmt.Fprintf(output, "synara_database_connections%s %d\n", labels(map[string]string{"state": "max_open"}), stats.MaxOpenConnections)
		fmt.Fprintf(output, "synara_database_connections%s %d\n", labels(map[string]string{"state": "open"}), stats.OpenConnections)
		fmt.Fprintf(output, "synara_database_connections%s %d\n", labels(map[string]string{"state": "in_use"}), stats.InUse)
		fmt.Fprintf(output, "synara_database_connections%s %d\n", labels(map[string]string{"state": "idle"}), stats.Idle)
		writeHelp(output, "synara_database_connection_wait_total", "Database connection pool waits.", "counter")
		fmt.Fprintf(output, "synara_database_connection_wait_total %d\n", stats.WaitCount)
		writeHelp(output, "synara_database_connection_wait_seconds_total", "Cumulative database connection pool wait duration.", "counter")
		fmt.Fprintf(output, "synara_database_connection_wait_seconds_total %s\n", formatFloat(stats.WaitDuration.Seconds()))
	}
	writeHelp(output, "synara_outbox_pending", "Unpublished authoritative outbox message count.", "gauge")
	fmt.Fprintf(output, "synara_outbox_pending %d\n", outboxPending)
	writeHelp(output, "synara_outbox_retrying", "Outbox messages waiting for another publish attempt.", "gauge")
	fmt.Fprintf(output, "synara_outbox_retrying %d\n", outboxRetrying)
	writeHelp(output, "synara_outbox_dead_letter", "Outbox messages that exhausted publish attempts.", "gauge")
	fmt.Fprintf(output, "synara_outbox_dead_letter %d\n", outboxDeadLetter)
	writeHelp(output, "synara_outbox_oldest_pending_seconds", "Age of the oldest pending outbox message.", "gauge")
	fmt.Fprintf(output, "synara_outbox_oldest_pending_seconds %s\n", formatFloat(oldestOutboxSeconds))
	writeExecutionGenerationOutcomeGauge(
		output,
		startupSamples,
		"synara_execution_generation_start_outcomes",
		"Authoritative immutable Execution generation count by whether Control Plane start was reached, termination won first, or the outcome is still pending.",
		"started",
		"terminal-before-start",
		func(sample executionStartupSample) *time.Time { return sample.StartedAt },
	)
	writeExecutionGenerationOutcomeGauge(
		output,
		startupSamples,
		"synara_execution_generation_provider_ready_outcomes",
		"Authoritative immutable Execution generation count by whether the Provider emitted session.started, termination won first, or readiness is still pending.",
		"ready",
		"terminal-before-ready",
		func(sample executionStartupSample) *time.Time { return sample.ProviderReadyAt },
	)
	writeExecutionGenerationDurationHistogram(
		output,
		startupSamples,
		"synara_execution_generation_startup_duration_seconds",
		"Authoritative Worker claim-to-Control Plane execution.started latency by recovery reason and target kind.",
		func(sample executionStartupSample) *time.Time { return sample.LeasedAt },
		func(sample executionStartupSample) *time.Time { return sample.StartedAt },
	)
	writeExecutionGenerationDurationHistogram(
		output,
		startupSamples,
		"synara_execution_generation_provider_ready_duration_seconds",
		"Authoritative Recovery Bundle creation-to-Provider session.started latency by recovery reason and target kind.",
		func(sample executionStartupSample) *time.Time { return &sample.BundleCreatedAt },
		func(sample executionStartupSample) *time.Time { return sample.ProviderReadyAt },
	)
	writeProviderResumeMetrics(output, claimResumeDecisions, runtimeResumeFallbacks)
	if err := r.writeExecutionGenerationFactMetrics(ctx, output, now); err != nil {
		return err
	}
	if err := r.writeWorkerIncarnationFactMetrics(ctx, output, now); err != nil {
		return err
	}
	if err := r.writeDistributedRoutingLeadershipBillingMetrics(ctx, output, now); err != nil {
		return err
	}
	writeHelp(output, "synara_metrics_collection_success", "Whether authoritative database metrics were collected successfully.", "gauge")
	output.WriteString("synara_metrics_collection_success 1\n")
	return nil
}

func (r *Registry) providerCredentialAccessLeaseCounts(
	ctx context.Context,
	now time.Time,
) ([]providerCredentialAccessLeaseCount, error) {
	// The immutable Grant supplies generation lineage while the current
	// Credential row supplies revocation, rotation, and hard-expiry truth. The
	// access window itself can remain usable briefly after semantic refresh has
	// closed, matching an access-token/refresh-token split.
	const accessState = `CASE
		WHEN credential.id IS NULL
			OR credential.revoked_at IS NOT NULL
			OR credential.version <> access_grant.credential_version
			OR (credential.expires_at IS NOT NULL AND credential.expires_at <= ?)
			THEN 'credential-unavailable'
		WHEN (access_session.absolute_expires_at IS NOT NULL AND access_session.absolute_expires_at <= ?)
			OR lease.provider_credential_access_expires_at <= ?
			OR (lease.provider_credential_hard_expires_at IS NOT NULL AND lease.provider_credential_hard_expires_at <= ?)
			THEN 'expired'
		WHEN lease.provider_credential_refresh_deadline_at <= ?
			THEN 'refresh-window-closed'
		ELSE 'active'
	END`
	var rows []providerCredentialAccessLeaseCount
	err := r.db.WithContext(ctx).
		Table("worker_leases AS lease").
		Select(accessState+" AS access_state, COUNT(*) AS count", now, now, now, now, now).
		Joins(`JOIN execution_provider_credential_grants AS access_grant
			ON access_grant.tenant_id = lease.tenant_id
			AND access_grant.id = lease.provider_credential_grant_id`).
		Joins(`JOIN agent_executions AS access_execution
			ON access_execution.tenant_id = lease.tenant_id
			AND access_execution.id = lease.execution_id`).
		Joins(`JOIN agent_sessions AS access_session
			ON access_session.tenant_id = access_execution.tenant_id
			AND access_session.id = access_execution.session_id`).
		Joins(`LEFT JOIN provider_credentials AS credential
			ON credential.tenant_id = access_grant.tenant_id
			AND credential.id = access_grant.credential_id`).
		Where("lease.provider_credential_grant_id IS NOT NULL").
		Group("access_state").
		Order("access_state").
		Scan(&rows).Error
	return rows, err
}

func groupedCounts(ctx context.Context, db *gorm.DB, table, statusColumn, kindExpression string) ([]groupedCount, error) {
	return groupedCountsWhere(ctx, db, table, statusColumn, kindExpression, "")
}

func groupedCountsWhere(
	ctx context.Context,
	db *gorm.DB,
	table, statusColumn, kindExpression, condition string,
	arguments ...any,
) ([]groupedCount, error) {
	var rows []groupedCount
	query := db.WithContext(ctx).Table(table)
	if condition != "" {
		query = query.Where(condition, arguments...)
	}
	err := query.Select(statusColumn + " AS status, " + kindExpression + ", COUNT(*) AS count").
		Group(statusColumn + ", target_kind").Order(statusColumn + ", target_kind").Scan(&rows).Error
	return rows, err
}

func valueCountsWhere(
	ctx context.Context,
	db *gorm.DB,
	table, column, condition string,
	arguments ...any,
) ([]valueCount, error) {
	var rows []valueCount
	query := db.WithContext(ctx).Table(table)
	if condition != "" {
		query = query.Where(condition, arguments...)
	}
	err := query.Select(column + " AS value, COUNT(*) AS count").
		Group(column).Order(column).Scan(&rows).Error
	return rows, err
}

func pairedCountsWhere(
	ctx context.Context,
	db *gorm.DB,
	table, firstColumn, secondColumn, condition string,
	arguments ...any,
) ([]pairedValueCount, error) {
	var rows []pairedValueCount
	query := db.WithContext(ctx).Table(table)
	if condition != "" {
		query = query.Where(condition, arguments...)
	}
	err := query.Select(firstColumn + " AS first, " + secondColumn + " AS second, COUNT(*) AS count").
		Group(firstColumn + ", " + secondColumn).Order(firstColumn + ", " + secondColumn).Scan(&rows).Error
	return rows, err
}

func writeGroupedGauge(output *bytes.Buffer, name, help string, rows []groupedCount) {
	writeHelp(output, name, help, "gauge")
	for _, row := range rows {
		fmt.Fprintf(output, "%s%s %d\n", name, labels(map[string]string{"status": row.Status, "target_kind": row.TargetKind}), row.Count)
	}
}

func writeSingleLabelGauge(output *bytes.Buffer, name, help, label string, rows []valueCount) {
	writeHelp(output, name, help, "gauge")
	for _, row := range rows {
		fmt.Fprintf(output, "%s%s %d\n", name, labels(map[string]string{label: row.Value}), row.Count)
	}
}

func writePairedGauge(output *bytes.Buffer, name, help, firstLabel, secondLabel string, rows []pairedValueCount) {
	writeHelp(output, name, help, "gauge")
	for _, row := range rows {
		fmt.Fprintf(output, "%s%s %d\n", name, labels(map[string]string{firstLabel: row.First, secondLabel: row.Second}), row.Count)
	}
}

func (r *Registry) executionStartupSamples(ctx context.Context) ([]executionStartupSample, error) {
	if r.db.Migrator().HasTable("execution_generation_facts") {
		type durableStartupRow struct {
			RecoveryReason          string     `gorm:"column:recovery_reason"`
			TargetKind              string     `gorm:"column:target_kind"`
			ExecutionID             uuid.UUID  `gorm:"column:execution_id"`
			Generation              int64      `gorm:"column:generation"`
			FactBundleCreatedAt     *time.Time `gorm:"column:fact_bundle_created_at"`
			RecoveryBundleCreatedAt *time.Time `gorm:"column:recovery_bundle_created_at"`
			DispatchRequestedAt     *time.Time `gorm:"column:dispatch_requested_at"`
			FactCreatedAt           time.Time  `gorm:"column:fact_created_at"`
			LeasedAt                *time.Time `gorm:"column:leased_at"`
			StartedAt               *time.Time `gorm:"column:started_at"`
			ProviderReadyAt         *time.Time `gorm:"column:provider_ready_at"`
			TerminalAt              *time.Time `gorm:"column:terminal_at"`
		}
		var factRows []durableStartupRow
		if err := r.db.WithContext(ctx).
			Table("execution_generation_facts AS facts").
			Select(`facts.recovery_reason,
				facts.target_kind,
				facts.execution_id,
				facts.generation,
				facts.bundle_created_at AS fact_bundle_created_at,
				bundles.created_at AS recovery_bundle_created_at,
				facts.dispatch_requested_at,
				facts.created_at AS fact_created_at,
				facts.leased_at,
				facts.execution_started_at AS started_at,
				facts.provider_ready_at,
				facts.terminal_at`).
			Joins(`LEFT JOIN execution_recovery_bundles AS bundles
				ON bundles.tenant_id = facts.tenant_id
				AND bundles.execution_id = facts.execution_id
				AND bundles.generation = facts.generation`).
			Order("facts.execution_id, facts.generation").
			Scan(&factRows).Error; err != nil {
			return nil, err
		}
		durableRows := make([]executionStartupSample, 0, len(factRows))
		for _, row := range factRows {
			baseline := row.FactCreatedAt
			if row.DispatchRequestedAt != nil {
				baseline = *row.DispatchRequestedAt
			}
			if row.RecoveryBundleCreatedAt != nil {
				baseline = *row.RecoveryBundleCreatedAt
			}
			if row.FactBundleCreatedAt != nil {
				baseline = *row.FactBundleCreatedAt
			}
			durableRows = append(durableRows, executionStartupSample{
				RecoveryReason: row.RecoveryReason, TargetKind: row.TargetKind,
				ExecutionID: row.ExecutionID, Generation: row.Generation,
				BundleCreatedAt: baseline, LeasedAt: row.LeasedAt, StartedAt: row.StartedAt,
				ProviderReadyAt: row.ProviderReadyAt, TerminalAt: row.TerminalAt,
			})
		}
		return durableRows, nil
	}

	var rows []executionStartupSample
	if err := r.db.WithContext(ctx).Table("execution_recovery_bundles AS bundles").
		Select(
			`bundles.recovery_reason,
			 execution.target_kind,
			 bundles.execution_id,
			 bundles.generation,
			 bundles.created_at AS bundle_created_at`,
		).
		Joins(
			"JOIN agent_executions AS execution ON execution.tenant_id = bundles.tenant_id AND execution.id = bundles.execution_id",
		).
		Order("bundles.execution_id, bundles.generation").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	type lifecycleEvent struct {
		ExecutionID uuid.UUID `gorm:"column:execution_id"`
		Generation  int64     `gorm:"column:generation"`
		EventType   string    `gorm:"column:event_type"`
		OccurredAt  time.Time `gorm:"column:occurred_at"`
	}
	var events []lifecycleEvent
	if err := r.db.WithContext(ctx).Table("session_events AS lifecycle").
		Select("lifecycle.execution_id, lifecycle.generation, lifecycle.event_type, lifecycle.occurred_at").
		Joins(`JOIN execution_recovery_bundles AS bundles
			ON bundles.tenant_id = lifecycle.tenant_id
			AND bundles.execution_id = lifecycle.execution_id
			AND bundles.generation = lifecycle.generation`).
		Where(
			"lifecycle.execution_id IS NOT NULL AND lifecycle.generation IS NOT NULL AND lifecycle.event_type IN ?",
			[]string{
				"execution.leased",
				"execution.started",
				"session.started",
				"execution.completed",
				"execution.failed",
				"execution.cancelled",
				"execution.interrupted",
				"execution.recovering",
			},
		).
		Order("lifecycle.execution_id, lifecycle.generation, lifecycle.occurred_at, lifecycle.event_id").
		Scan(&events).Error; err != nil {
		return nil, err
	}
	type generationKey struct {
		executionID uuid.UUID
		generation  int64
	}
	rowByGeneration := make(map[generationKey]*executionStartupSample, len(rows))
	for index := range rows {
		rowByGeneration[generationKey{executionID: rows[index].ExecutionID, generation: rows[index].Generation}] = &rows[index]
	}
	for _, event := range events {
		row := rowByGeneration[generationKey{executionID: event.ExecutionID, generation: event.Generation}]
		if row == nil {
			continue
		}
		switch event.EventType {
		case "execution.leased":
			setEarlierTime(&row.LeasedAt, event.OccurredAt)
		case "execution.started":
			setEarlierTime(&row.StartedAt, event.OccurredAt)
		case "session.started":
			setEarlierTime(&row.ProviderReadyAt, event.OccurredAt)
		case "execution.completed", "execution.failed", "execution.cancelled", "execution.interrupted", "execution.recovering":
			setEarlierTime(&row.TerminalAt, event.OccurredAt)
		}
	}
	return rows, nil
}

func setEarlierTime(target **time.Time, value time.Time) {
	if *target == nil || value.Before(**target) {
		copy := value
		*target = &copy
	}
}

func writeExecutionGenerationDurationHistogram(
	output *bytes.Buffer,
	samples []executionStartupSample,
	name, help string,
	startedAt func(executionStartupSample) *time.Time,
	observedAt func(executionStartupSample) *time.Time,
) {
	type metricKey struct {
		recoveryReason string
		targetKind     string
	}
	metrics := make(map[metricKey]startupDurationMetric)
	type startupKey struct {
		executionID uuid.UUID
		generation  int64
	}
	seen := make(map[startupKey]struct{})
	for _, sample := range samples {
		key := startupKey{executionID: sample.ExecutionID, generation: sample.Generation}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		started := startedAt(sample)
		completedAt := observedAt(sample)
		if started == nil || completedAt == nil || completedAt.Before(*started) {
			continue
		}
		metricKey := metricKey{recoveryReason: sample.RecoveryReason, targetKind: sample.TargetKind}
		metric := metrics[metricKey]
		seconds := completedAt.Sub(*started).Seconds()
		metric.count++
		metric.sum += seconds
		for index, upperBound := range executionStartupBuckets {
			if seconds <= upperBound {
				metric.buckets[index]++
			}
		}
		metrics[metricKey] = metric
	}
	writeHelp(output, name, help, "histogram")
	keys := make([]metricKey, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].targetKind != keys[j].targetKind {
			return keys[i].targetKind < keys[j].targetKind
		}
		return keys[i].recoveryReason < keys[j].recoveryReason
	})
	for _, key := range keys {
		metric := metrics[key]
		labelSet := labels(map[string]string{"recovery_reason": key.recoveryReason, "target_kind": key.targetKind})
		for index, upperBound := range executionStartupBuckets {
			fmt.Fprintf(
				output,
				name+"_bucket%s %d\n",
				addLabel(labelSet, "le", strconv.FormatFloat(upperBound, 'g', -1, 64)),
				metric.buckets[index],
			)
		}
		fmt.Fprintf(
			output,
			name+"_bucket%s %d\n",
			addLabel(labelSet, "le", "+Inf"),
			metric.count,
		)
		fmt.Fprintf(output, "%s_sum%s %s\n", name, labelSet, formatFloat(metric.sum))
		fmt.Fprintf(output, "%s_count%s %d\n", name, labelSet, metric.count)
	}
}

func writeExecutionGenerationOutcomeGauge(
	output *bytes.Buffer,
	samples []executionStartupSample,
	name, help, successOutcome, terminalOutcome string,
	observedAt func(executionStartupSample) *time.Time,
) {
	type metricKey struct {
		recoveryReason string
		targetKind     string
		outcome        string
	}
	counts := make(map[metricKey]uint64)
	for _, sample := range samples {
		outcome := "pending"
		if completedAt := observedAt(sample); completedAt != nil && !completedAt.Before(sample.BundleCreatedAt) {
			outcome = successOutcome
		} else if sample.TerminalAt != nil && !sample.TerminalAt.Before(sample.BundleCreatedAt) {
			outcome = terminalOutcome
		}
		counts[metricKey{
			recoveryReason: sample.RecoveryReason,
			targetKind:     sample.TargetKind,
			outcome:        outcome,
		}]++
	}
	writeHelp(output, name, help, "gauge")
	keys := make([]metricKey, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].targetKind != keys[j].targetKind {
			return keys[i].targetKind < keys[j].targetKind
		}
		if keys[i].recoveryReason != keys[j].recoveryReason {
			return keys[i].recoveryReason < keys[j].recoveryReason
		}
		return keys[i].outcome < keys[j].outcome
	})
	for _, key := range keys {
		fmt.Fprintf(output, "%s%s %d\n", name, labels(map[string]string{
			"recovery_reason": key.recoveryReason,
			"target_kind":     key.targetKind,
			"outcome":         key.outcome,
		}), counts[key])
	}
}

func (r *Registry) providerResumeMetrics(
	ctx context.Context,
) (map[providerResumeClaimDecisionMetricKey]uint64, map[providerResumeRuntimeFallbackMetricKey]uint64, error) {
	claimDecisions := make(map[providerResumeClaimDecisionMetricKey]uint64)
	var leasedEvents []persistence.SessionEvent
	if err := r.db.WithContext(ctx).
		Select("payload").
		Where("event_type = ?", "execution.leased").
		Find(&leasedEvents).Error; err != nil {
		return nil, nil, err
	}
	for _, event := range leasedEvents {
		resume, ok := event.Payload["providerResume"].(map[string]any)
		if !ok {
			continue
		}
		claimDecisions[providerResumeClaimDecisionMetricKey{
			TargetKind:       boundedTargetKind(metricString(event.Payload["targetKind"])),
			SelectedStrategy: boundedResumeStrategy(metricString(resume["selectedStrategy"])),
			ReasonCode:       boundedResumeReasonCode(metricString(resume["reasonCode"])),
		}]++
	}

	runtimeFallbacks := make(map[providerResumeRuntimeFallbackMetricKey]uint64)
	if r.db.Migrator().HasTable("execution_generation_facts") {
		var rows []struct {
			Provider   string `gorm:"column:provider"`
			ReasonCode string `gorm:"column:reason_code"`
			Count      uint64 `gorm:"column:count"`
		}
		if err := r.db.WithContext(ctx).Table("execution_generation_facts").
			Select(`COALESCE(resume_fallback_provider, provider) AS provider,
				resume_fallback_reason_code AS reason_code, COUNT(*) AS count`).
			Where("resume_fallback_reason_code IS NOT NULL").
			Group("COALESCE(resume_fallback_provider, provider), resume_fallback_reason_code").
			Scan(&rows).Error; err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			if (row.Provider != "codex" && row.Provider != "claudeAgent") ||
				(row.ReasonCode != "session_resume_invalid" && row.ReasonCode != "session_resume_expired") {
				continue
			}
			runtimeFallbacks[providerResumeRuntimeFallbackMetricKey{
				Provider: row.Provider, ReasonCode: row.ReasonCode,
			}] += row.Count
		}
		return claimDecisions, runtimeFallbacks, nil
	}
	// Compatibility path for a database that has not yet applied migration
	// 000059. Once the durable fact table exists, repeated warning events are
	// deliberately ignored so a Generation contributes at most once.
	var warningEvents []persistence.SessionEvent
	if err := r.db.WithContext(ctx).
		Select("payload").
		Where("event_type = ?", "runtime.warning").
		Find(&warningEvents).Error; err != nil {
		return nil, nil, err
	}
	for _, event := range warningEvents {
		if !executions.IsProviderResumeFallbackRuntimeWarningPayload(event.Payload) {
			continue
		}
		detail, ok := event.Payload["detail"].(map[string]any)
		if !ok {
			continue
		}
		provider := metricString(detail["provider"])
		reasonCode := metricString(detail["reasonCode"])
		if (provider != "codex" && provider != "claudeAgent") ||
			(reasonCode != "session_resume_invalid" && reasonCode != "session_resume_expired") {
			continue
		}
		runtimeFallbacks[providerResumeRuntimeFallbackMetricKey{
			Provider: provider, ReasonCode: reasonCode,
		}]++
	}
	return claimDecisions, runtimeFallbacks, nil
}

func writeProviderResumeMetrics(
	output *bytes.Buffer,
	claimDecisions map[providerResumeClaimDecisionMetricKey]uint64,
	runtimeFallbacks map[providerResumeRuntimeFallbackMetricKey]uint64,
) {
	writeHelp(
		output,
		"synara_provider_resume_claim_decisions",
		"Authoritative execution.leased Provider resume decisions by bounded strategy, reason, and target kind.",
		"gauge",
	)
	claimKeys := make([]providerResumeClaimDecisionMetricKey, 0, len(claimDecisions))
	for key := range claimDecisions {
		claimKeys = append(claimKeys, key)
	}
	sort.Slice(claimKeys, func(i, j int) bool {
		if claimKeys[i].TargetKind != claimKeys[j].TargetKind {
			return claimKeys[i].TargetKind < claimKeys[j].TargetKind
		}
		if claimKeys[i].SelectedStrategy != claimKeys[j].SelectedStrategy {
			return claimKeys[i].SelectedStrategy < claimKeys[j].SelectedStrategy
		}
		return claimKeys[i].ReasonCode < claimKeys[j].ReasonCode
	})
	for _, key := range claimKeys {
		fmt.Fprintf(output, "synara_provider_resume_claim_decisions%s %d\n", labels(map[string]string{
			"target_kind":       key.TargetKind,
			"selected_strategy": key.SelectedStrategy,
			"reason_code":       key.ReasonCode,
		}), claimDecisions[key])
	}

	writeHelp(
		output,
		"synara_provider_resume_runtime_fallbacks",
		"Validated Provider runtime native-cursor fallbacks to authoritative history by bounded Provider and reason.",
		"gauge",
	)
	fallbackKeys := make([]providerResumeRuntimeFallbackMetricKey, 0, len(runtimeFallbacks))
	for key := range runtimeFallbacks {
		fallbackKeys = append(fallbackKeys, key)
	}
	sort.Slice(fallbackKeys, func(i, j int) bool {
		if fallbackKeys[i].Provider != fallbackKeys[j].Provider {
			return fallbackKeys[i].Provider < fallbackKeys[j].Provider
		}
		return fallbackKeys[i].ReasonCode < fallbackKeys[j].ReasonCode
	})
	for _, key := range fallbackKeys {
		fmt.Fprintf(output, "synara_provider_resume_runtime_fallbacks%s %d\n", labels(map[string]string{
			"provider": key.Provider, "reason_code": key.ReasonCode,
		}), runtimeFallbacks[key])
	}
}

func metricString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func boundedTargetKind(value string) string {
	switch value {
	case "local", "docker", "ssh", "kubernetes":
		return value
	default:
		return "other"
	}
}

func boundedResumeStrategy(value string) string {
	switch value {
	case "native-cursor", "authoritative-history":
		return value
	default:
		return "other"
	}
}

func boundedResumeReasonCode(value string) string {
	switch value {
	case "cursor_absent",
		"cursor_quarantined",
		"resume_strategy_authoritative_history",
		"cursor_binding_unavailable",
		"cursor_cipher_unavailable",
		"cursor_open_failed",
		"cursor_binding_mismatch",
		"cursor_payload_invalid",
		"cursor_lineage_mismatch",
		"cursor_issued_in_future",
		"cursor_expired",
		"cursor_usable",
		"cursor_legacy_unbound",
		"cursor_authentication_failed",
		"cursor_envelope_unsupported",
		"cursor_unusable":
		return value
	default:
		return "other"
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

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneMap[K comparable, V any](values map[K]V) map[K]V {
	cloned := make(map[K]V, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
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
