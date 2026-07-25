package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestGatherUsesBoundedRoutePatternsAndAuthoritativeState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	models := []any{
		&persistence.AgentExecution{}, &persistence.WorkerInstance{}, &persistence.WorkerLease{},
		&persistence.ExecutionTarget{}, &persistence.OutboxMessage{}, &persistence.SSEConnectionLease{},
		&persistence.LoginSession{}, &persistence.Artifact{}, &persistence.AgentSession{},
		&persistence.ExecutionRecoveryBundle{}, &persistence.ExecutionSuspendAttempt{}, &persistence.SessionEvent{},
		&persistence.ProviderCredential{}, &persistence.ExecutionProviderCredentialGrant{},
		&persistence.ExecutionTargetHealth{}, &persistence.ExecutionFailoverAttempt{}, &persistence.ReconcilerLease{},
		&persistence.WorkerIncarnationFact{}, &persistence.BillingProviderTariff{},
		&persistence.BillingEstimatedUsageCharge{}, &persistence.BillingActualInvoiceImport{},
		&persistence.BillingActualInvoiceLine{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	workerID := uuid.New()
	staleWorkerID := uuid.New()
	freshWorkerID := uuid.New()
	revokedWorkerID := uuid.New()
	executionID := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	recoveryTenantID := uuid.New()
	userID := uuid.New()
	sessionID := uuid.New()
	waitingSessionID := uuid.New()
	suspendedSessionID := uuid.New()
	turnID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.ExecutionTarget{ID: targetID, Kind: "docker", Name: "test", Status: "active", Capabilities: map[string]any{}}).Error; err != nil {
		t.Fatal(err)
	}
	for _, session := range []persistence.AgentSession{
		{
			ID: sessionID, TenantID: uuid.New(), OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: userID, Title: "active", Status: "active", Visibility: "private", Provider: "openai",
			ExecutionTargetID: targetID, LastEventSequence: 2, ResourceState: "active", MeaningfulActivityAt: now,
			WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30,
			WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: waitingSessionID, TenantID: uuid.New(), OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: userID, Title: "waiting", Status: "active", Visibility: "private", Provider: "openai",
			ExecutionTargetID: targetID, LastEventSequence: 0, ResourceState: "waiting", MeaningfulActivityAt: now,
			WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30,
			WarmPoolMode: "balanced", CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: suspendedSessionID, TenantID: uuid.New(), OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: userID, Title: "suspended", Status: "suspended", Visibility: "private", Provider: "openai",
			ExecutionTargetID: targetID, LastEventSequence: 0, ResourceState: "suspended", MeaningfulActivityAt: now,
			WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30,
			WarmPoolMode: "low-latency", CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
		Namespace: "test", PodName: "worker", Version: "test", ProtocolVersion: 1, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("hash"), Status: "ready",
		AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, worker := range []persistence.WorkerInstance{
		{
			ID: staleWorkerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
			Namespace: "test", PodName: "stale-worker", Version: "test", ProtocolVersion: 2,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte("stale-hash"), Status: "online", AdministrativeStatus: "active", RegisteredAt: now,
			LastHeartbeatAt: now.Add(-2 * time.Minute),
		},
		{
			ID: freshWorkerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
			Namespace: "test", PodName: "fresh-worker", Version: "test", ProtocolVersion: 2,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte("fresh-hash"), Status: "online", AdministrativeStatus: "active", RegisteredAt: now,
			LastHeartbeatAt: now,
		},
		{
			ID: revokedWorkerID, ExecutionTargetID: targetID, TargetKind: "docker", ClusterID: "test",
			Namespace: "test", PodName: "revoked-worker", Version: "test", ProtocolVersion: 2,
			Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true,
			AuthTokenHash: []byte("revoked-hash"), Status: "online", AdministrativeStatus: "revoked", RegisteredAt: now,
			LastHeartbeatAt: now.Add(-2 * time.Minute), RevokedAt: &now,
		},
	} {
		if err := db.Create(&worker).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: recoveryTenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1,
		Status: "running", ExecutionTargetID: targetID, TargetKind: "docker", WorkerID: &workerID,
		Generation: 2, RequestedBy: uuid.New(), QueuedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerLease{
		ExecutionID: executionID, TenantID: uuid.New(), WorkerID: workerID,
		WorkerIncarnation: 1, WorkerInstanceUID: uuid.NewString(), Generation: 1,
		LeaseTokenHash: []byte("hash"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	deadAt := now
	if err := db.Create(&persistence.OutboxMessage{
		ID: uuid.New(), Topic: "execution.queued", MessageKey: uuid.NewString(),
		Payload: map[string]any{}, Headers: map[string]any{}, Attempts: 2,
		AvailableAt: now, CreatedAt: now.Add(-time.Minute), DeadLetteredAt: &deadAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.OutboxMessage{
		ID: uuid.New(), Topic: "execution.queued", MessageKey: uuid.NewString(),
		Payload: map[string]any{}, Headers: map[string]any{}, Attempts: 1,
		AvailableAt: now, CreatedAt: now.Add(-30 * time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	startedInitialAt := now.Add(-40 * time.Second)
	startedResumeAt := now.Add(-10 * time.Second)
	terminalRecoveryBundleAt := now.Add(-4 * time.Second)
	for _, bundle := range []persistence.ExecutionRecoveryBundle{
		{
			ID: uuid.New(), TenantID: recoveryTenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
			AuthoritativeHistorySequence: 1, Payload: map[string]any{"schemaVersion": 1},
			PayloadSHA256: strings.Repeat("a", 64), CreatedAt: startedInitialAt.Add(-4 * time.Second),
		},
		{
			ID: uuid.New(), TenantID: recoveryTenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, Generation: 2, SchemaVersion: 1, RecoveryReason: "suspend-resume",
			AuthoritativeHistorySequence: 2, Payload: map[string]any{"schemaVersion": 1},
			PayloadSHA256: strings.Repeat("b", 64), CreatedAt: startedResumeAt.Add(-12 * time.Second),
		},
		{
			ID: uuid.New(), TenantID: recoveryTenantID, SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, Generation: 3, SchemaVersion: 1, RecoveryReason: "execution-recovery",
			AuthoritativeHistorySequence: 3, Payload: map[string]any{"schemaVersion": 1},
			PayloadSHA256: strings.Repeat("c", 64), CreatedAt: terminalRecoveryBundleAt,
		},
	} {
		if err := db.Create(&bundle).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []persistence.SessionEvent{
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 8, EventID: uuid.New(), EventVersion: 2, EventType: "execution.leased",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(1), Payload: map[string]any{},
			OccurredAt: startedInitialAt.Add(-2 * time.Second),
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 1, EventID: uuid.New(), EventVersion: 1, EventType: "execution.started",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(1), Payload: map[string]any{},
			OccurredAt: startedInitialAt,
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 2, EventID: uuid.New(), EventVersion: 1, EventType: "execution.started",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(2), Payload: map[string]any{},
			OccurredAt: startedResumeAt,
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 3, EventID: uuid.New(), EventVersion: 2, EventType: "session.started",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(1), Payload: map[string]any{},
			OccurredAt: startedInitialAt.Add(2 * time.Second),
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 4, EventID: uuid.New(), EventVersion: 2, EventType: "session.started",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(2), Payload: map[string]any{},
			OccurredAt: startedResumeAt.Add(time.Second),
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 5, EventID: uuid.New(), EventVersion: 2, EventType: "execution.leased",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(2), Payload: map[string]any{
				"targetKind": "docker",
				"providerResume": map[string]any{
					"selectedStrategy": "authoritative-history", "reasonCode": "cursor_expired",
				},
			},
			OccurredAt: startedResumeAt.Add(-time.Second),
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 6, EventID: uuid.New(), EventVersion: 2, EventType: "runtime.warning",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(2), Payload: map[string]any{
				"message": "Native Provider resume failed before turn activity; authoritative history was selected.",
				"detail": map[string]any{
					"kind": "session_resume", "attemptedStrategy": "native-cursor",
					"selectedStrategy": "authoritative-history", "outcome": "fallback_selected",
					"reasonCode": "session_resume_expired", "fallbackSafety": "before_turn_activity",
					"authoritativeHistorySequence": float64(2), "provider": "codex",
				},
			},
			OccurredAt: startedResumeAt,
		},
		{
			TenantID: recoveryTenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID,
			Sequence: 7, EventID: uuid.New(), EventVersion: 2, EventType: "execution.recovering",
			ActorType: "worker", ExecutionID: &executionID, Generation: pointerInt64(3), Payload: map[string]any{},
			OccurredAt: terminalRecoveryBundleAt.Add(time.Second),
		},
	} {
		if err := db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	abortedAt := now.Add(-5 * time.Second)
	failureCode := "checkpoint_deadline_exceeded"
	for _, attempt := range []persistence.ExecutionSuspendAttempt{
		{
			ID: uuid.New(), TenantID: uuid.New(), SessionID: sessionID, TurnID: turnID,
			ExecutionID: executionID, WorkerID: workerID, Generation: 1, Reason: "waiting-keepalive",
			Status: "completed", RequestedAt: startedResumeAt.Add(-30 * time.Second),
			CheckpointDeadlineAt: startedResumeAt.Add(-28 * time.Second), CompletedAt: pointerTime(startedResumeAt.Add(-29 * time.Second)),
		},
		{
			ID: uuid.New(), TenantID: uuid.New(), SessionID: waitingSessionID, TurnID: uuid.New(),
			ExecutionID: uuid.New(), WorkerID: workerID, Generation: 1, Reason: "waiting-keepalive",
			Status: "aborted", RequestedAt: abortedAt.Add(-15 * time.Second),
			CheckpointDeadlineAt: abortedAt.Add(30 * time.Second), AbortedAt: pointerTime(abortedAt), FailureCode: &failureCode,
		},
	} {
		if err := db.Create(&attempt).Error; err != nil {
			t.Fatal(err)
		}
	}

	registry := New(db, Config{SessionIdleTTL: 7 * 24 * time.Hour, WorkerHeartbeatTimeout: 90 * time.Second})
	registry.ObserveHTTP("GET", "GET /v1/sessions/{sessionID}", 200, 25*time.Millisecond, "")
	registry.ObserveHTTP("GET", "/v1/sessions/"+executionID.String(), 404, 10*time.Millisecond, "")
	registry.ObserveBackground("docker", now, nil)
	registry.ObserveArtifact("complete", 128, nil)
	registry.ObserveSSECatchup(20*time.Millisecond, 3, nil)
	registry.ObserveSSELimit("user")
	payload, err := registry.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`route="/v1/sessions/{sessionID}"`, `route="unmatched"`,
		`synara_workers{status="ready",target_kind="docker"} 1`,
		`synara_workers_administrative{status="active",target_kind="docker"} 3`,
		`synara_workers_administrative{status="revoked",target_kind="docker"} 1`,
		`synara_stale_workers{status="online",target_kind="docker"} 1`,
		`synara_executions{status="running",target_kind="docker"} 1`,
		`synara_sessions_resource_state{resource_state="active",session_status="active"} 1`,
		`synara_sessions_resource_state{resource_state="waiting",session_status="active"} 1`,
		`synara_sessions_resource_state{resource_state="suspended",session_status="suspended"} 1`,
		`synara_sessions_warm_pool_mode{session_status="active",warm_pool_mode="balanced"} 1`,
		`synara_sessions_warm_pool_mode{session_status="active",warm_pool_mode="disabled"} 1`,
		`synara_sessions_warm_pool_mode{session_status="suspended",warm_pool_mode="low-latency"} 1`,
		`synara_resource_suspend_attempts{reason="waiting-keepalive",status="aborted"} 1`,
		`synara_resource_suspend_attempts{reason="waiting-keepalive",status="completed"} 1`,
		`synara_execution_recovery_bundles{recovery_reason="initial-claim"} 1`,
		`synara_execution_recovery_bundles{recovery_reason="suspend-resume"} 1`,
		`synara_execution_recovery_bundles{recovery_reason="execution-recovery"} 1`,
		`synara_execution_generation_start_outcomes{outcome="started",recovery_reason="initial-claim",target_kind="docker"} 1`,
		`synara_execution_generation_start_outcomes{outcome="started",recovery_reason="suspend-resume",target_kind="docker"} 1`,
		`synara_execution_generation_start_outcomes{outcome="terminal-before-start",recovery_reason="execution-recovery",target_kind="docker"} 1`,
		`synara_execution_generation_provider_ready_outcomes{outcome="ready",recovery_reason="suspend-resume",target_kind="docker"} 1`,
		`synara_execution_generation_provider_ready_outcomes{outcome="terminal-before-ready",recovery_reason="execution-recovery",target_kind="docker"} 1`,
		`synara_execution_generation_startup_duration_seconds_count{recovery_reason="initial-claim",target_kind="docker"} 1`,
		`synara_execution_generation_startup_duration_seconds_count{recovery_reason="suspend-resume",target_kind="docker"} 1`,
		`synara_execution_generation_provider_ready_duration_seconds_count{recovery_reason="initial-claim",target_kind="docker"} 1`,
		`synara_execution_generation_provider_ready_duration_seconds_count{recovery_reason="suspend-resume",target_kind="docker"} 1`,
		`synara_provider_resume_claim_decisions{reason_code="cursor_expired",selected_strategy="authoritative-history",target_kind="docker"} 1`,
		`synara_provider_resume_runtime_fallbacks{provider="codex",reason_code="session_resume_expired"} 1`,
		`synara_provider_credential_access_leases{state="active"} 0`,
		`synara_provider_credential_access_leases{state="credential-unavailable"} 0`,
		`synara_worker_leases{state="active"} 1`, `synara_metrics_collection_success 1`,
		`synara_outbox_pending 1`, `synara_outbox_retrying 1`,
		`synara_outbox_dead_letter 1`, `synara_outbox_oldest_pending_seconds`,
		`synara_sse_connections{state="active"} 0`, `synara_artifact_ready_bytes 0`,
		`synara_database_connections{state="open"}`, `synara_artifact_operations_total{operation="complete",result="success"} 1`,
		`synara_sse_catchup_events_total 3`, `synara_sse_connection_rejections_total{scope="user"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	if strings.Contains(metrics, executionID.String()) {
		t.Fatalf("metrics leaked a high-cardinality execution identifier:\n%s", metrics)
	}
}

func TestBoundedReconcilerLeaseNameIncludesBillingImportScheduler(t *testing.T) {
	if got := boundedReconcilerLeaseName("synara:billing-import-scheduler"); got != "billing-import" {
		t.Fatalf("billing import scheduler lease label = %q, want billing-import", got)
	}
}

func pointerInt64(value int64) *int64 { return &value }

func pointerTime(value time.Time) *time.Time { return &value }

func TestObserveHTTPDerivesBoundedLoginLeaseFencingAndEventMetrics(t *testing.T) {
	registry := New(nil)
	registry.ObserveHTTP("POST", "POST /v1/auth/dev-login", 200, 10*time.Millisecond, "")
	registry.ObserveHTTP("GET", "GET /v1/auth/sso/{connectionID}/callback", 401, 15*time.Millisecond, "oidc_id_token_invalid")
	registry.ObserveHTTP("POST", "POST /v1/auth/sso/{connectionID}/callback", 303, 20*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/renew", 200, 5*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/renew", 409, 7*time.Millisecond, "generation_fenced")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/events", 201, 25*time.Millisecond, "")
	registry.ObserveHTTP("POST", "POST /v1/workers/executions/{executionID}/events", 409, 30*time.Millisecond, "lease_expired")
	registry.ObserveHTTP("POST", "POST /v1/workers/heartbeat", 409, 3*time.Millisecond, "worker_incarnation_fenced")
	registry.ObserveHTTP("POST", "POST /v1/workers/heartbeat", 401, 3*time.Millisecond, "worker_token_revoked")

	var output bytes.Buffer
	registry.writeProcessMetrics(&output)
	metrics := output.String()
	for _, expected := range []string{
		`synara_login_attempts_total{method="dev",result="success"} 1`,
		`synara_login_attempts_total{method="oidc",result="failure"} 1`,
		`synara_login_attempts_total{method="saml",result="success"} 1`,
		`synara_worker_lease_renewals_total{result="success"} 1`,
		`synara_worker_lease_renewals_total{result="rejected"} 1`,
		`synara_worker_fencing_rejections_total{operation="lease-renew"} 1`,
		`synara_worker_fencing_rejections_total{operation="session-event"} 1`,
		`synara_worker_fencing_rejections_total{operation="heartbeat"} 2`,
		`synara_session_event_append_duration_seconds_count{result="success"} 1`,
		`synara_session_event_append_duration_seconds_count{result="rejected"} 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{"oidc_id_token_invalid", "generation_fenced", "lease_expired", "worker_incarnation_fenced", "worker_token_revoked"} {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked unbounded problem code %q:\n%s", forbidden, metrics)
		}
	}
}

func TestProviderResumeMetricLabelsAreBounded(t *testing.T) {
	for _, testCase := range []struct {
		name string
		got  string
		want string
	}{
		{name: "target", got: boundedTargetKind("tenant-controlled-target"), want: "other"},
		{name: "strategy", got: boundedResumeStrategy("provider-extension"), want: "other"},
		{name: "reason", got: boundedResumeReasonCode("secret-bearing-error"), want: "other"},
		{name: "known reason", got: boundedResumeReasonCode("cursor_expired"), want: "cursor_expired"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Fatalf("bounded label = %q, want %q", testCase.got, testCase.want)
			}
		})
	}
}

func TestProviderCredentialAccessLeaseCountsUseFrozenGrantAndSemanticWindows(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ProviderCredential{},
		&persistence.ExecutionProviderCredentialGrant{},
		&persistence.AgentSession{},
		&persistence.AgentExecution{},
		&persistence.WorkerLease{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 25, 8, 30, 0, 0, time.UTC)
	tenantID := uuid.New()
	workerID := uuid.New()
	createdBy := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	targetID := uuid.New()
	revokedAt := now.Add(-time.Minute)
	credentials := []persistence.ProviderCredential{
		{ID: uuid.New(), TenantID: tenantID, Name: "usable", Purpose: "provider", Provider: "openai", CredentialType: "api_key", Version: 1, CreatedBy: createdBy, UpdatedBy: createdBy, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), TenantID: tenantID, Name: "revoked", Purpose: "provider", Provider: "openai", CredentialType: "api_key", Version: 1, CreatedBy: createdBy, UpdatedBy: createdBy, CreatedAt: now, UpdatedAt: now, RevokedAt: &revokedAt},
		{ID: uuid.New(), TenantID: tenantID, Name: "rotated", Purpose: "provider", Provider: "openai", CredentialType: "api_key", Version: 2, CreatedBy: createdBy, UpdatedBy: createdBy, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&credentials).Error; err != nil {
		t.Fatal(err)
	}
	type accessFixture struct {
		credentialIndex int
		grantVersion    int
		accessExpiresAt time.Time
		refreshDeadline time.Time
		absoluteExpired bool
	}
	fixtures := []accessFixture{
		{credentialIndex: 0, grantVersion: 1, accessExpiresAt: now.Add(5 * time.Minute), refreshDeadline: now.Add(time.Minute)},
		{credentialIndex: 0, grantVersion: 1, accessExpiresAt: now.Add(5 * time.Minute), refreshDeadline: now.Add(-time.Second)},
		{credentialIndex: 0, grantVersion: 1, accessExpiresAt: now.Add(-time.Second), refreshDeadline: now.Add(-time.Minute)},
		{credentialIndex: 1, grantVersion: 1, accessExpiresAt: now.Add(5 * time.Minute), refreshDeadline: now.Add(time.Minute)},
		{credentialIndex: 2, grantVersion: 1, accessExpiresAt: now.Add(5 * time.Minute), refreshDeadline: now.Add(time.Minute)},
		{credentialIndex: 0, grantVersion: 1, accessExpiresAt: now.Add(5 * time.Minute), refreshDeadline: now.Add(time.Minute), absoluteExpired: true},
	}
	for _, fixture := range fixtures {
		executionID := uuid.New()
		sessionID := uuid.New()
		turnID := uuid.New()
		grantID := uuid.New()
		var absoluteExpiresAt *time.Time
		if fixture.absoluteExpired {
			expiredAt := now.Add(-time.Second)
			absoluteExpiresAt = &expiredAt
		}
		if err := db.Create(&persistence.AgentSession{
			ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: createdBy, Title: "access metric", Status: "active", Visibility: "private",
			Provider: "openai", ExecutionTargetID: targetID, LastEventSequence: 7,
			MeaningfulActivitySequence: 7, ResourceState: "active", MeaningfulActivityAt: now.Add(-time.Minute),
			AbsoluteExpiresAt: absoluteExpiresAt, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
			WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.AgentExecution{
			ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1,
			Status: "running", ExecutionTargetID: targetID, TargetKind: "docker", WorkerID: &workerID,
			Generation: 3, RequestedBy: createdBy, QueuedAt: now.Add(-time.Minute),
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.ExecutionProviderCredentialGrant{
			ID: grantID, TenantID: tenantID, ExecutionID: executionID, Generation: 3,
			CredentialID: credentials[fixture.credentialIndex].ID, CredentialVersion: fixture.grantVersion, CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		serial := int64(1)
		activitySequence := int64(7)
		issuedAt := now.Add(-10 * time.Minute)
		renewedAt := now.Add(-time.Minute)
		if err := db.Create(&persistence.WorkerLease{
			ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID, WorkerIncarnation: 1,
			WorkerInstanceUID: uuid.NewString(), Generation: 3, ProviderCredentialGrantID: &grantID,
			ProviderCredentialAccessSerial: &serial, ProviderCredentialActivitySequence: &activitySequence,
			ProviderCredentialActivityAt: &renewedAt, ProviderCredentialAccessIssuedAt: &issuedAt,
			ProviderCredentialAccessRenewedAt: &renewedAt, ProviderCredentialAccessExpiresAt: &fixture.accessExpiresAt,
			ProviderCredentialRefreshDeadlineAt: &fixture.refreshDeadline,
			LeaseTokenHash:                      []byte("hash"), AcquiredAt: issuedAt, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	rows, err := New(db).providerCredentialAccessLeaseCounts(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int64, len(rows))
	for _, row := range rows {
		got[row.State] = row.Count
	}
	want := map[string]int64{
		"active": 1, "refresh-window-closed": 1, "expired": 2, "credential-unavailable": 2,
	}
	if len(got) != len(want) {
		t.Fatalf("access Lease states = %#v, want %#v", got, want)
	}
	for state, count := range want {
		if got[state] != count {
			t.Fatalf("access Lease state %q = %d, want %d (all states %#v)", state, got[state], count, got)
		}
	}
}
