package executions

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestObserveKubernetesExecutionPodPersistsTimelineAndIndependentFailures(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "pod-fact-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	targetID := uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Kubernetes Pod facts", DefaultBranch: "main", Visibility: "private",
			CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "Kubernetes Pod fact target", Status: "active",
			Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Kubernetes Pod facts",
			Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: targetID,
			ResourceState: "provisioning", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900,
			SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
			CreatedAt: now, UpdatedAt: now,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "queued", InputText: "Provision Pod", TurnKind: "message",
			RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes",
			Generation: 0, RequestedBy: domain.UserID, QueuedAt: now,
		},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	if err := store.DB().Create(&persistence.ExecutionGenerationFact{
		TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
		SessionID: sessionID, TurnID: turnID, ExecutionTargetID: targetID,
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &now,
		ProviderResumeStrategy: "authoritative-history", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	service := &Service{db: store.DB()}
	podUID := uuid.NewString()
	base := executiontargets.KubernetesExecutionPodObservation{
		TenantID: domain.TenantID, ExecutionTargetID: targetID, ExecutionID: executionID, Generation: 1,
		Namespace: "synara-test", PodName: "synara-exec-test-g1",
		PendingFailureThreshold: 30 * time.Second, PodCreatedAt: now.Add(2 * time.Second),
	}
	observations := []executiontargets.KubernetesExecutionPodObservation{
		withKubernetesPodObservation(base, now.Add(time.Second), "ApplyFailed", "", executiontargets.KubernetesPodFailureApplyFailed, "api-status-429"),
		withKubernetesPodObservation(base, now.Add(2*time.Second), "Applied", "", "", ""),
		withKubernetesPodObservation(base, now.Add(3*time.Second), "Pending", podUID, executiontargets.KubernetesPodFailureImagePull, "image-pull-backoff"),
		withKubernetesPodObservation(base, now.Add(34*time.Second), "Pending", podUID, executiontargets.KubernetesPodFailureImagePull, "image-pull-backoff"),
		withKubernetesPodObservation(base, now.Add(40*time.Second), "Running", podUID, "", ""),
		withKubernetesPodObservation(base, now.Add(50*time.Second), "Failed", podUID, executiontargets.KubernetesPodFailureOOMKilled, "oom-killed"),
	}
	for _, observation := range observations {
		if err := service.ObserveKubernetesExecutionPod(ctx, observation); err != nil {
			t.Fatalf("observe %s/%s: %v", observation.Phase, observation.FailureClass, err)
		}
	}

	var generation persistence.ExecutionGenerationFact
	if err := store.DB().Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1,
	).Take(&generation).Error; err != nil {
		t.Fatal(err)
	}
	if generation.PodProvisioningStartedAt == nil || !generation.PodProvisioningStartedAt.Equal(now.Add(time.Second)) ||
		generation.PodPendingSinceAt == nil || !generation.PodPendingSinceAt.Equal(now.Add(3*time.Second)) ||
		generation.PodRunningAt == nil || !generation.PodRunningAt.Equal(now.Add(40*time.Second)) ||
		generation.PodLastObservedAt == nil || !generation.PodLastObservedAt.Equal(now.Add(50*time.Second)) {
		t.Fatalf("unexpected Kubernetes Pod generation timeline: %#v", generation)
	}

	var failures []persistence.ExecutionGenerationPodFailureFact
	if err := store.DB().Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1,
	).Order("failure_class").Find(&failures).Error; err != nil {
		t.Fatal(err)
	}
	if len(failures) != 4 {
		t.Fatalf("failure facts = %#v, want apply, image-pull, Pending timeout, and OOM", failures)
	}
	byClass := make(map[string]persistence.ExecutionGenerationPodFailureFact, len(failures))
	for _, failure := range failures {
		byClass[failure.FailureClass] = failure
	}
	applyFailure := byClass[executiontargets.KubernetesPodFailureApplyFailed]
	if applyFailure.PodUID != nil || applyFailure.ReasonCode != "api-status-429" {
		t.Fatalf("apply failure fact = %#v", applyFailure)
	}
	imagePull := byClass[executiontargets.KubernetesPodFailureImagePull]
	if imagePull.PodUID == nil || *imagePull.PodUID != podUID ||
		!imagePull.FirstObservedAt.Equal(now.Add(3*time.Second)) ||
		!imagePull.LastObservedAt.Equal(now.Add(34*time.Second)) {
		t.Fatalf("image-pull failure fact = %#v", imagePull)
	}
	if timeout := byClass[executiontargets.KubernetesPodFailurePendingTimeout]; timeout.ReasonCode != "pending-threshold-exceeded" || timeout.PodUID == nil {
		t.Fatalf("Pending timeout fact = %#v", timeout)
	}
	if oom := byClass[executiontargets.KubernetesPodFailureOOMKilled]; oom.PodUID == nil || oom.ReasonCode != "oom-killed" {
		t.Fatalf("OOM failure fact = %#v", oom)
	}
}

func withKubernetesPodObservation(
	base executiontargets.KubernetesExecutionPodObservation,
	observedAt time.Time,
	phase, podUID, failureClass, failureReason string,
) executiontargets.KubernetesExecutionPodObservation {
	base.ObservedAt = observedAt
	base.Phase = phase
	base.PodUID = podUID
	if podUID == "" {
		base.PodCreatedAt = time.Time{}
	}
	base.FailureClass = failureClass
	base.FailureReasonCode = failureReason
	return base
}

func TestNormalizeKubernetesExecutionPodObservationRejectsUnboundedFailureReason(t *testing.T) {
	_, err := normalizeKubernetesExecutionPodObservation(executiontargets.KubernetesExecutionPodObservation{
		TenantID: uuid.New(), ExecutionTargetID: uuid.New(), ExecutionID: uuid.New(), Generation: 1,
		Namespace: "synara-test", PodName: "worker", PodUID: uuid.NewString(), Phase: "Failed",
		FailureClass:            executiontargets.KubernetesPodFailureGeneric,
		FailureReasonCode:       "raw message with tenant-controlled cardinality",
		PendingFailureThreshold: time.Minute, ObservedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected a non-canonical failure reason to be rejected")
	}
}

func TestPostgresObserveKubernetesExecutionPodReplaysFailureClass(t *testing.T) {
	db, _ := isolatedPostgresTestDBs(t)
	fixture := seedExecutionFixtureWithoutCleanup(t, db)
	service := integrationService(t, db)
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	dispatchedAt := execution.QueuedAt.UTC()
	if err := db.Create(&persistence.ExecutionGenerationFact{
		TenantID: fixture.TenantID, ExecutionID: fixture.ExecutionID, Generation: 1,
		SessionID: fixture.SessionID, TurnID: fixture.TurnID, ExecutionTargetID: fixture.TargetID,
		TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
		WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &dispatchedAt,
		ProviderResumeStrategy: "authoritative-history", CreatedAt: dispatchedAt, UpdatedAt: dispatchedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	podUID := uuid.NewString()
	base := executiontargets.KubernetesExecutionPodObservation{
		TenantID: fixture.TenantID, ExecutionTargetID: fixture.TargetID,
		ExecutionID: fixture.ExecutionID, Generation: 1,
		Namespace: "postgres-test", PodName: "postgres-worker", PodUID: podUID,
		Phase: "Pending", FailureClass: executiontargets.KubernetesPodFailureImagePull,
		FailureReasonCode: "image-pull-backoff", PendingFailureThreshold: 30 * time.Second,
		PodCreatedAt: dispatchedAt.Add(time.Second),
	}
	for _, observedAt := range []time.Time{dispatchedAt.Add(time.Second), dispatchedAt.Add(32 * time.Second)} {
		observation := base
		observation.ObservedAt = observedAt
		if err := service.ObserveKubernetesExecutionPod(context.Background(), observation); err != nil {
			t.Fatal(err)
		}
	}
	var failures []persistence.ExecutionGenerationPodFailureFact
	if err := db.Where(
		"tenant_id = ? AND execution_id = ? AND generation = ?",
		fixture.TenantID,
		fixture.ExecutionID,
		1,
	).Order("failure_class").Find(&failures).Error; err != nil {
		t.Fatal(err)
	}
	if len(failures) != 2 {
		t.Fatalf("PostgreSQL failure facts = %#v, want image-pull plus Pending timeout", failures)
	}
	byClass := make(map[string]persistence.ExecutionGenerationPodFailureFact, len(failures))
	for _, failure := range failures {
		byClass[failure.FailureClass] = failure
	}
	imagePull := byClass[executiontargets.KubernetesPodFailureImagePull]
	if !imagePull.FirstObservedAt.Equal(dispatchedAt.Add(time.Second)) ||
		!imagePull.LastObservedAt.Equal(dispatchedAt.Add(32*time.Second)) {
		t.Fatalf("PostgreSQL replayed image-pull fact = %#v", imagePull)
	}
	if pending := byClass[executiontargets.KubernetesPodFailurePendingTimeout]; pending.ReasonCode != "pending-threshold-exceeded" || pending.PodUID == nil {
		t.Fatalf("PostgreSQL Pending timeout fact = %#v", pending)
	}
}
