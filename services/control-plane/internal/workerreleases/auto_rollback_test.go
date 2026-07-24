package workerreleases

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAutoRollbackTriggersForReleaseAttributableFailureThreshold(t *testing.T) {
	fixture := newReleaseFixture(t)
	first, second := startAutoRollbackCanary(t, fixture, AutoRollbackInput{
		Enabled: true, ObservationWindowSeconds: 60, MinimumExecutions: 4,
		FailureThreshold: 2, FailureRatePercent: 50,
	})

	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "completed", "")
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "completed", "")
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "provider_version_incompatible")
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "protocol_violation")

	controller := newTestAutoRollbackController(fixture)
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	var policy persistence.WorkerReleasePolicy
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 3 || policy.PromotedRevisionID != first.ID || policy.CanaryRevisionID != nil {
		t.Fatalf("automatic rollback policy = %#v", policy)
	}
	window := fixture.loadAutoRollbackWindow(t, 2)
	if window.Status != "triggered" || window.DecisionReason == nil || *window.DecisionReason != "release_failure_threshold_exceeded" {
		t.Fatalf("automatic rollback window = %#v", window)
	}
	var systemAuditCount, eventCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ? AND actor_type = ? AND actor_id IS NULL", fixture.tenantID, "worker_release.auto_rollback_triggered", "system").
		Count(&systemAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ?", fixture.tenantID, "worker.release.auto-rollback-triggered").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if systemAuditCount != 1 || eventCount != 1 {
		t.Fatalf("automatic rollback audit/event counts = %d/%d", systemAuditCount, eventCount)
	}
	if err := fixture.db.Model(&persistence.WorkerReleaseAutoRollbackWindow{}).
		Where("id = ?", window.ID).Update("minimum_executions", 9).Error; err == nil {
		t.Fatal("SQLite safety accepted mutation of automatic rollback policy evidence")
	}
	if err := fixture.db.Delete(&persistence.WorkerReleaseAutoRollbackWindow{}, "id = ?", window.ID).Error; err == nil {
		t.Fatal("SQLite safety accepted deletion of automatic rollback evidence")
	}
}

func TestAutoRollbackIgnoresExternalProviderFailures(t *testing.T) {
	fixture := newReleaseFixture(t)
	_, second := startAutoRollbackCanary(t, fixture, AutoRollbackInput{
		Enabled: true, ObservationWindowSeconds: 60, MinimumExecutions: 1,
		FailureThreshold: 1, FailureRatePercent: 1,
	})
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "provider_unavailable")
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "provider_authentication_failed")
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "rate_limited")

	controller := newTestAutoRollbackController(fixture)
	window := fixture.loadAutoRollbackWindow(t, 2)
	controller.now = func() time.Time { return window.ExpiresAt.Add(time.Second) }
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	window = fixture.loadAutoRollbackWindow(t, 2)
	if window.Status != "expired" || window.DecisionReason != nil {
		t.Fatalf("external Provider failures changed release policy: %#v", window)
	}
	var policy persistence.WorkerReleasePolicy
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 2 || policy.CanaryRevisionID == nil || *policy.CanaryRevisionID != second.ID {
		t.Fatalf("policy changed for ignored Provider failures: %#v", policy)
	}
}

func TestAutoRollbackPersistsPendingDecisionUntilExecutionDrains(t *testing.T) {
	fixture := newReleaseFixture(t)
	first, second := startAutoRollbackCanary(t, fixture, AutoRollbackInput{
		Enabled: true, ObservationWindowSeconds: 60, MinimumExecutions: 1,
		FailureThreshold: 1, FailureRatePercent: 1,
	})
	fixture.finishReleaseExecution(t, second.ID, ChannelCanary, "failed", "provider_not_installed")
	activeExecutionID := fixture.seedQueuedExecution(t, second.ID, ChannelCanary)
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", activeExecutionID).
		Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}

	controller := newTestAutoRollbackController(fixture)
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if window := fixture.loadAutoRollbackWindow(t, 2); window.Status != "rollback-pending" {
		t.Fatalf("blocked automatic rollback window = %#v", window)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", activeExecutionID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if window := fixture.loadAutoRollbackWindow(t, 2); window.Status != "triggered" {
		t.Fatalf("retried automatic rollback window = %#v", window)
	}
	var policy persistence.WorkerReleasePolicy
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 3 || policy.PromotedRevisionID != first.ID {
		t.Fatalf("retried automatic rollback policy = %#v", policy)
	}
}

func TestAutoRollbackTriggersForNewCandidateWorkerIncompatibility(t *testing.T) {
	fixture := newReleaseFixture(t)
	first, _ := startAutoRollbackCanary(t, fixture, AutoRollbackInput{
		Enabled: true, ObservationWindowSeconds: 60, MinimumExecutions: 100,
		FailureThreshold: 100, FailureRatePercent: 100,
	})
	checkedAt := time.Now().UTC().Add(time.Second)
	reason := "candidate protocol range is incompatible"
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.secondWorkerID).
		Updates(map[string]any{
			"compatibility_status": "incompatible", "compatibility_reason": reason,
			"compatibility_checked_at": checkedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	controller := newTestAutoRollbackController(fixture)
	controller.now = func() time.Time { return checkedAt.Add(time.Second) }
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	window := fixture.loadAutoRollbackWindow(t, 2)
	if window.Status != "triggered" || window.DecisionReason == nil || *window.DecisionReason != "candidate_worker_incompatible" {
		t.Fatalf("incompatible Worker automatic rollback window = %#v", window)
	}
	var policy persistence.WorkerReleasePolicy
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.PromotedRevisionID != first.ID {
		t.Fatalf("incompatible Worker automatic rollback policy = %#v", policy)
	}
}

func TestAutoRollbackObservationContinuesAfterCanaryPromotion(t *testing.T) {
	fixture := newReleaseFixture(t)
	first, second := startAutoRollbackCanary(t, fixture, AutoRollbackInput{
		Enabled: true, ObservationWindowSeconds: 60, MinimumExecutions: 1,
		FailureThreshold: 1, FailureRatePercent: 1,
	})
	fixture.changePolicy(t, "promote", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 2, Reason: "canary accepted; continue promoted observation",
	}, "auto-promote-second")

	canaryWindow := fixture.loadAutoRollbackWindow(t, 2)
	promotedWindow := fixture.loadAutoRollbackWindow(t, 3)
	if canaryWindow.Status != "superseded" || promotedWindow.Status != "monitoring" ||
		promotedWindow.CandidateChannel != ChannelPromoted || promotedWindow.FallbackRevisionID != first.ID {
		t.Fatalf("carried automatic rollback windows = %#v / %#v", canaryWindow, promotedWindow)
	}
	fixture.finishReleaseExecution(t, second.ID, ChannelPromoted, "failed", "protocol_violation")

	controller := newTestAutoRollbackController(fixture)
	if err := controller.EvaluateOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var policy persistence.WorkerReleasePolicy
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 4 || policy.PromotedRevisionID != first.ID {
		t.Fatalf("post-promotion automatic rollback policy = %#v", policy)
	}
	if window := fixture.loadAutoRollbackWindow(t, 3); window.Status != "triggered" {
		t.Fatalf("post-promotion automatic rollback window = %#v", window)
	}
}

func TestCanaryAutoRollbackCanBeDisabledAndValidatesThresholds(t *testing.T) {
	fixture := newReleaseFixture(t)
	first := fixture.createRevision(t, fixture.firstManifestID, "initial", "disabled-release-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "candidate", "disabled-release-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "disabled-promote-first")

	_, err := fixture.service.StartCanary(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, second.ID,
		PolicyChangeInput{
			ExpectedPolicyVersion: 1, CanaryPercent: 10, Reason: "invalid automatic rollback policy",
			AutoRollback: &AutoRollbackInput{
				Enabled: true, ObservationWindowSeconds: 59, MinimumExecutions: 2,
				FailureThreshold: 3, FailureRatePercent: 50,
			},
		},
		"invalid-auto-rollback", "request-invalid-auto-rollback", "127.0.0.1",
	)
	assertProblem(t, err, 400, "invalid_worker_release_auto_rollback")

	fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 10, Reason: "operator-approved manual observation",
		AutoRollback: &AutoRollbackInput{Enabled: false},
	}, "disabled-canary-second")
	var count int64
	if err := fixture.db.Model(&persistence.WorkerReleaseAutoRollbackWindow{}).
		Where("execution_target_id = ?", fixture.targetID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("disabled canary created %d automatic rollback windows", count)
	}
}

func startAutoRollbackCanary(
	t *testing.T,
	fixture releaseFixture,
	config AutoRollbackInput,
) (Revision, Revision) {
	t.Helper()
	first := fixture.createRevision(t, fixture.firstManifestID, "initial", "auto-release-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "candidate", "auto-release-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "auto-promote-first")
	fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 100, Reason: "start automatic rollback observation",
		AutoRollback: &config,
	}, "auto-canary-second")
	return first, second
}

func newTestAutoRollbackController(fixture releaseFixture) *AutoRollbackController {
	return NewAutoRollbackController(
		fixture.service,
		AutoRollbackControllerConfig{Enabled: true, Interval: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func (f releaseFixture) finishReleaseExecution(
	t *testing.T,
	revisionID uuid.UUID,
	channel, status, failureCode string,
) uuid.UUID {
	t.Helper()
	executionID := f.seedQueuedExecution(t, revisionID, channel)
	updates := map[string]any{"status": status, "finished_at": time.Now().UTC()}
	if failureCode != "" {
		updates["failure_code"] = failureCode
	}
	if err := f.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
	return executionID
}

func (f releaseFixture) loadAutoRollbackWindow(
	t *testing.T,
	policyVersion int64,
) persistence.WorkerReleaseAutoRollbackWindow {
	t.Helper()
	var window persistence.WorkerReleaseAutoRollbackWindow
	if err := f.db.Where("execution_target_id = ? AND policy_version = ?", f.targetID, policyVersion).
		Take(&window).Error; err != nil {
		t.Fatal(err)
	}
	return window
}
