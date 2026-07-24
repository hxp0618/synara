package workerreleases

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const autoRollbackAdvisoryLock = "synara:worker-release-auto-rollback"

var attributableReleaseFailureCodes = map[string]struct{}{
	"protocol_violation":            {},
	"provider_not_installed":        {},
	"provider_version_incompatible": {},
}

type autoRollbackConfiguration struct {
	ObservationWindow  time.Duration
	MinimumExecutions  int
	FailureThreshold   int
	FailureRatePercent int
}

type AutoRollbackControllerConfig struct {
	Enabled  bool
	Interval time.Duration
	Observer BackgroundObserver
}

type BackgroundObserver interface {
	ObserveBackground(kind string, started time.Time, err error)
}

type AutoRollbackController struct {
	db      *gorm.DB
	service *Service
	config  AutoRollbackControllerConfig
	logger  *slog.Logger
	now     func() time.Time
}

func NewAutoRollbackController(
	service *Service,
	config AutoRollbackControllerConfig,
	logger *slog.Logger,
) *AutoRollbackController {
	return &AutoRollbackController{
		db: service.db, service: service, config: config, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (c *AutoRollbackController) Run(ctx context.Context) {
	if !c.config.Enabled {
		return
	}
	interval := c.config.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := time.Now()
		err := c.EvaluateOnce(ctx)
		if c.config.Observer != nil {
			c.config.Observer.ObserveBackground("worker-release-auto-rollback", started, err)
		}
		if err != nil && ctx.Err() == nil && c.logger != nil {
			c.logger.Error("Worker release automatic rollback evaluation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *AutoRollbackController) EvaluateOnce(ctx context.Context) error {
	if !c.config.Enabled {
		return nil
	}
	release, acquired, err := persistence.TryAdvisoryLock(ctx, c.db, autoRollbackAdvisoryLock)
	if err != nil {
		return problem.Wrap(500, "worker_release_auto_rollback_lock_failed", "Worker release auto-rollback coordination failed.", err)
	}
	if !acquired {
		return nil
	}
	defer release()

	windows := make([]persistence.WorkerReleaseAutoRollbackWindow, 0)
	if err := c.db.WithContext(ctx).
		Where("status IN ?", []string{"monitoring", "rollback-pending"}).
		Order("expires_at, execution_target_id, policy_version").
		Find(&windows).Error; err != nil {
		return problem.Wrap(500, "worker_release_auto_rollback_load_failed", "Worker release auto-rollback windows could not be loaded.", err)
	}
	var failures []error
	for index := range windows {
		if err := c.evaluateWindow(ctx, &windows[index]); err != nil {
			failures = append(failures, fmt.Errorf("window %s: %w", windows[index].ID, err))
		}
	}
	return errors.Join(failures...)
}

func (c *AutoRollbackController) evaluateWindow(
	ctx context.Context,
	window *persistence.WorkerReleaseAutoRollbackWindow,
) error {
	var policy persistence.WorkerReleasePolicy
	err := c.db.WithContext(ctx).Where("execution_target_id = ?", window.ExecutionTargetID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && policy.PolicyVersion != window.PolicyVersion) {
		return c.supersedeWindow(ctx, *window)
	}
	if err != nil {
		return problem.Wrap(500, "worker_release_auto_rollback_policy_lookup_failed", "Worker release policy could not be loaded for automatic rollback.", err)
	}
	if window.Status == "rollback-pending" {
		return c.triggerRollback(ctx, *window)
	}

	now := c.now().UTC()
	evidence, reason, err := c.evaluateEvidence(ctx, *window, now)
	if err != nil {
		return err
	}
	if reason == "" {
		if now.Before(window.ExpiresAt) {
			return nil
		}
		updated := c.db.WithContext(ctx).Model(&persistence.WorkerReleaseAutoRollbackWindow{}).
			Where("id = ? AND status = ?", window.ID, "monitoring").
			Select("status", "evidence", "updated_at").
			Updates(&persistence.WorkerReleaseAutoRollbackWindow{
				Status: "expired", Evidence: evidence, UpdatedAt: now,
			})
		if updated.Error != nil {
			return problem.Wrap(500, "worker_release_auto_rollback_expire_failed", "Worker release auto-rollback window could not be expired.", updated.Error)
		}
		return nil
	}

	updated := c.db.WithContext(ctx).Model(&persistence.WorkerReleaseAutoRollbackWindow{}).
		Where("id = ? AND status = ?", window.ID, "monitoring").
		Select("status", "decision_reason", "evidence", "decision_at", "updated_at").
		Updates(&persistence.WorkerReleaseAutoRollbackWindow{
			Status: "rollback-pending", DecisionReason: &reason,
			Evidence: evidence, DecisionAt: &now, UpdatedAt: now,
		})
	if updated.Error != nil {
		return problem.Wrap(500, "worker_release_auto_rollback_decision_failed", "Worker release auto-rollback decision could not be persisted.", updated.Error)
	}
	if updated.RowsAffected != 1 {
		return nil
	}
	window.Status = "rollback-pending"
	window.DecisionReason = &reason
	window.DecisionAt = &now
	window.Evidence = evidence
	return c.triggerRollback(ctx, *window)
}

func (c *AutoRollbackController) evaluateEvidence(
	ctx context.Context,
	window persistence.WorkerReleaseAutoRollbackWindow,
	now time.Time,
) (map[string]any, string, error) {
	type outcomeCount struct {
		Status      string
		FailureCode *string
		Total       int
	}
	until := now
	if until.After(window.ExpiresAt) {
		until = window.ExpiresAt
	}
	rows := make([]outcomeCount, 0)
	if err := c.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Select("status, failure_code, COUNT(*) AS total").
		Where(
			"execution_target_id = ? AND worker_release_revision_id = ? AND worker_release_channel = ? AND queued_at >= ? AND queued_at <= ? AND status IN ?",
			window.ExecutionTargetID, window.CandidateRevisionID, window.CandidateChannel,
			window.StartedAt, until, []string{"completed", "failed"},
		).
		Group("status, failure_code").Scan(&rows).Error; err != nil {
		return nil, "", problem.Wrap(500, "worker_release_auto_rollback_execution_probe_failed", "Candidate execution outcomes could not be inspected.", err)
	}

	successCount, attributableFailureCount, ignoredFailureCount := 0, 0, 0
	attributableCodes := map[string]int{}
	ignoredCodes := map[string]int{}
	for _, row := range rows {
		if row.Status == "completed" {
			successCount += row.Total
			continue
		}
		code := "unknown"
		if row.FailureCode != nil && *row.FailureCode != "" {
			code = *row.FailureCode
		}
		if _, attributable := attributableReleaseFailureCodes[code]; attributable {
			attributableFailureCount += row.Total
			attributableCodes[code] += row.Total
		} else {
			ignoredFailureCount += row.Total
			ignoredCodes[code] += row.Total
		}
	}

	var revision persistence.WorkerReleaseRevision
	if err := c.db.WithContext(ctx).Where("id = ? AND execution_target_id = ?", window.CandidateRevisionID, window.ExecutionTargetID).Take(&revision).Error; err != nil {
		return nil, "", problem.Wrap(500, "worker_release_auto_rollback_revision_lookup_failed", "Candidate Worker release revision could not be loaded.", err)
	}
	var incompatibleWorkers int64
	if err := c.db.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where(
			"execution_target_id = ? AND current_manifest_id = ? AND compatibility_status = ? AND ((registered_at >= ? AND registered_at <= ?) OR (compatibility_checked_at >= ? AND compatibility_checked_at <= ?))",
			window.ExecutionTargetID, revision.WorkerManifestID, "incompatible",
			window.StartedAt, until, window.StartedAt, until,
		).Count(&incompatibleWorkers).Error; err != nil {
		return nil, "", problem.Wrap(500, "worker_release_auto_rollback_worker_probe_failed", "Candidate Worker compatibility could not be inspected.", err)
	}

	relevantExecutionCount := successCount + attributableFailureCount
	evidence := map[string]any{
		"candidateRevisionId": window.CandidateRevisionID, "candidateChannel": window.CandidateChannel,
		"fallbackRevisionId": window.FallbackRevisionID, "startedAt": window.StartedAt, "expiresAt": window.ExpiresAt,
		"successCount": successCount, "attributableFailureCount": attributableFailureCount,
		"ignoredFailureCount": ignoredFailureCount, "relevantExecutionCount": relevantExecutionCount,
		"attributableFailureCodes": sortedCounts(attributableCodes), "ignoredFailureCodes": sortedCounts(ignoredCodes),
		"incompatibleWorkerCount": incompatibleWorkers, "minimumExecutions": window.MinimumExecutions,
		"failureThreshold": window.FailureThreshold, "failureRatePercent": window.FailureRatePercent,
	}
	if incompatibleWorkers > 0 {
		return evidence, "candidate_worker_incompatible", nil
	}
	if relevantExecutionCount >= window.MinimumExecutions &&
		attributableFailureCount >= window.FailureThreshold &&
		attributableFailureCount*100 >= window.FailureRatePercent*relevantExecutionCount {
		return evidence, "release_failure_threshold_exceeded", nil
	}
	return evidence, "", nil
}

func (c *AutoRollbackController) triggerRollback(
	ctx context.Context,
	window persistence.WorkerReleaseAutoRollbackWindow,
) error {
	activeTenantID := window.TenantID
	principal := identity.Principal{UserID: window.EnabledBy, ActiveTenantID: &activeTenantID}
	reason := "Automatic rollback triggered by the Worker release health policy."
	_, err := c.service.changePolicy(
		ctx, principal, window.TenantID, window.ExecutionTargetID, window.FallbackRevisionID,
		PolicyChangeInput{ExpectedPolicyVersion: window.PolicyVersion, Reason: reason},
		"worker-auto-rollback-"+window.ID.String(), "worker-auto-rollback-"+window.ID.String(), "", "rollback",
		policyChangeOptions{automaticWindowID: &window.ID},
	)
	if err == nil {
		return nil
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) {
		return err
	}
	switch apiError.Code {
	case "worker_release_active_executions", "worker_release_no_online_workers":
		// The persisted rollback-pending decision is retried after the active
		// execution drains or the fallback pool becomes healthy.
		return nil
	case "worker_release_policy_version_conflict", "worker_release_auto_rollback_superseded":
		return c.supersedeWindow(ctx, window)
	default:
		return err
	}
}

func (c *AutoRollbackController) supersedeWindow(
	ctx context.Context,
	window persistence.WorkerReleaseAutoRollbackWindow,
) error {
	if window.Status != "monitoring" && window.Status != "rollback-pending" {
		return nil
	}
	updated := c.db.WithContext(ctx).Model(&persistence.WorkerReleaseAutoRollbackWindow{}).
		Where("id = ? AND status = ?", window.ID, window.Status).
		Updates(map[string]any{"status": "superseded", "updated_at": c.now().UTC()})
	if updated.Error != nil {
		return problem.Wrap(500, "worker_release_auto_rollback_supersede_failed", "Worker release auto-rollback window could not be superseded.", updated.Error)
	}
	return nil
}

func normalizeAutoRollbackInput(action string, input *AutoRollbackInput) (*autoRollbackConfiguration, error) {
	if action != "canary" {
		return nil, nil
	}
	if input != nil && !input.Enabled {
		if input.ObservationWindowSeconds != 0 || input.MinimumExecutions != 0 || input.FailureThreshold != 0 || input.FailureRatePercent != 0 {
			return nil, problem.New(400, "invalid_worker_release_auto_rollback", "Disabled autoRollback must not include threshold settings.")
		}
		return nil, nil
	}
	config := autoRollbackConfiguration{
		ObservationWindow:  DefaultAutoRollbackObservationWindow,
		MinimumExecutions:  DefaultAutoRollbackMinimumExecutions,
		FailureThreshold:   DefaultAutoRollbackFailureThreshold,
		FailureRatePercent: DefaultAutoRollbackFailureRatePercent,
	}
	if input != nil {
		if input.ObservationWindowSeconds != 0 {
			config.ObservationWindow = time.Duration(input.ObservationWindowSeconds) * time.Second
		}
		if input.MinimumExecutions != 0 {
			config.MinimumExecutions = input.MinimumExecutions
		}
		if input.FailureThreshold != 0 {
			config.FailureThreshold = input.FailureThreshold
		}
		if input.FailureRatePercent != 0 {
			config.FailureRatePercent = input.FailureRatePercent
		}
	}
	if config.ObservationWindow < time.Minute || config.ObservationWindow > 24*time.Hour ||
		config.MinimumExecutions < 1 || config.MinimumExecutions > 10000 ||
		config.FailureThreshold < 1 || config.FailureThreshold > config.MinimumExecutions ||
		config.FailureRatePercent < 1 || config.FailureRatePercent > 100 {
		return nil, problem.New(400, "invalid_worker_release_auto_rollback", "autoRollback thresholds are outside the supported range.")
	}
	return &config, nil
}

func newAutoRollbackWindow(
	tenantID, targetID uuid.UUID,
	policyVersion int64,
	candidateRevisionID uuid.UUID,
	candidateChannel string,
	fallbackRevisionID, enabledBy uuid.UUID,
	startedAt time.Time,
	config autoRollbackConfiguration,
) persistence.WorkerReleaseAutoRollbackWindow {
	startedAt = startedAt.UTC()
	return persistence.WorkerReleaseAutoRollbackWindow{
		ID: uuid.New(), TenantID: tenantID, ExecutionTargetID: targetID, PolicyVersion: policyVersion,
		CandidateRevisionID: candidateRevisionID, CandidateChannel: candidateChannel,
		FallbackRevisionID: fallbackRevisionID, StartedAt: startedAt, ExpiresAt: startedAt.Add(config.ObservationWindow),
		MinimumExecutions: config.MinimumExecutions, FailureThreshold: config.FailureThreshold,
		FailureRatePercent: config.FailureRatePercent, EnabledBy: enabledBy,
		Status: "monitoring", Evidence: map[string]any{}, CreatedAt: startedAt, UpdatedAt: startedAt,
	}
}

func autoRollbackConfigurationFromWindow(window persistence.WorkerReleaseAutoRollbackWindow) autoRollbackConfiguration {
	return autoRollbackConfiguration{
		ObservationWindow: window.ExpiresAt.Sub(window.StartedAt), MinimumExecutions: window.MinimumExecutions,
		FailureThreshold: window.FailureThreshold, FailureRatePercent: window.FailureRatePercent,
	}
}

func projectAutoRollbackWindow(model persistence.WorkerReleaseAutoRollbackWindow) AutoRollbackWindow {
	return AutoRollbackWindow{
		ID: model.ID, PolicyVersion: model.PolicyVersion, CandidateRevisionID: model.CandidateRevisionID,
		CandidateChannel: model.CandidateChannel, FallbackRevisionID: model.FallbackRevisionID,
		StartedAt: model.StartedAt, ExpiresAt: model.ExpiresAt, MinimumExecutions: model.MinimumExecutions,
		FailureThreshold: model.FailureThreshold, FailureRatePercent: model.FailureRatePercent,
		Status: model.Status, DecisionReason: model.DecisionReason, Evidence: model.Evidence,
		DecisionAt: model.DecisionAt, EnabledBy: model.EnabledBy,
	}
}

type namedCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

func sortedCounts(counts map[string]int) []namedCount {
	result := make([]namedCount, 0, len(counts))
	for code, count := range counts {
		result = append(result, namedCount{Code: code, Count: count})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Code < result[j].Code })
	return result
}
