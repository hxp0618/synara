package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteRuntimeIsolationDecisionIsScopedAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "runtime-isolation-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	projectID, sessionID, turnID, executionID, targetID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Runtime isolation", Visibility: "organization", DefaultBranch: "main", CreatedBy: domain.UserID,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "kubernetes", Name: "gVisor", Status: "active", Capabilities: map[string]any{},
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Runtime isolation",
			Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: targetID,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "queued", InputText: "verify runtime isolation",
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
			Status: "queued", Attempt: 1, ExecutionTargetID: targetID, TargetKind: "kubernetes",
			RequestedBy: domain.UserID, QueuedAt: now,
		},
		&persistence.ExecutionGenerationFact{
			TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
			SessionID: sessionID, TurnID: turnID, ExecutionTargetID: targetID,
			TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
			DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range models {
		if err := store.DB().WithContext(ctx).Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	effectiveRuntime, effectiveProfile := "runc", "kubernetes-restricted-v1"
	decision := persistence.ExecutionRuntimeIsolationDecision{
		TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
		ExecutionTargetID: targetID, AllocationBackend: "native-pod",
		RequestedRuntime: "runc", RequestedProfile: effectiveProfile,
		EffectiveRuntime: &effectiveRuntime, EffectiveProfile: &effectiveProfile,
		PolicySource: "legacy-native", Decision: "selected", CreatedAt: now,
	}
	if err := store.DB().WithContext(ctx).Create(&decision).Error; err != nil {
		t.Fatalf("insert valid runtime isolation decision: %v", err)
	}
	if err := store.DB().WithContext(ctx).Model(&decision).Update("decision", "fallback").Error; err == nil {
		t.Fatal("runtime isolation decision was mutable")
	}
	if err := store.DB().WithContext(ctx).Delete(&decision).Error; err == nil {
		t.Fatal("runtime isolation decision was deletable")
	}
	var generationFact persistence.ExecutionGenerationFact
	if err := store.DB().WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1).
		Take(&generationFact).Error; err != nil {
		t.Fatal(err)
	}
	generationFact.Generation = 2
	if err := store.DB().WithContext(ctx).Create(&generationFact).Error; err != nil {
		t.Fatal(err)
	}
	missingEffective := decision
	missingEffective.Generation = 2
	missingEffective.EffectiveRuntime = nil
	missingEffective.EffectiveProfile = nil
	if err := store.DB().WithContext(ctx).Create(&missingEffective).Error; err == nil {
		t.Fatal("selected SQLite runtime isolation decision accepted NULL effective facts")
	}
	generationFact.Generation = 3
	if err := store.DB().WithContext(ctx).Create(&generationFact).Error; err != nil {
		t.Fatal(err)
	}
	gvisorRuntime, gvisorProfile, runtimeClass := "gvisor", "gvisor-sandboxed-v1", "synara-gvisor"
	invalidDigest := strings.Repeat("A", 64)
	expiresAt := now.Add(time.Minute)
	invalidGVisor := decision
	invalidGVisor.Generation = 3
	invalidGVisor.RequestedRuntime = gvisorRuntime
	invalidGVisor.RequestedProfile = gvisorProfile
	invalidGVisor.EffectiveRuntime = &gvisorRuntime
	invalidGVisor.EffectiveProfile = &gvisorProfile
	invalidGVisor.PolicySource = "target-explicit"
	invalidGVisor.RuntimeClassName = &runtimeClass
	invalidGVisor.AttestationDigest = &invalidDigest
	invalidGVisor.AttestedAt = &now
	invalidGVisor.AttestationExpiresAt = &expiresAt
	if err := store.DB().WithContext(ctx).Create(&invalidGVisor).Error; err == nil {
		t.Fatal("SQLite runtime isolation decision accepted a non-lower-hex attestation digest")
	}

	wrongTargetID := uuid.New()
	if err := store.DB().WithContext(ctx).Create(&persistence.ExecutionTarget{
		ID: wrongTargetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "kubernetes", Name: "wrong target", Status: "active", Capabilities: map[string]any{},
	}).Error; err != nil {
		t.Fatal(err)
	}
	invalid := decision
	invalid.Generation = 2
	invalid.ExecutionTargetID = wrongTargetID
	if err := store.DB().WithContext(ctx).Create(&invalid).Error; err == nil {
		t.Fatal("runtime isolation decision escaped its Generation target scope")
	}
}

func TestPostgresRuntimeIsolationDecisionAndObservationConstraints(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := db.Exec("SELECT set_config('search_path', current_schema() || ', public', false)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var releaseCompatibilityColumn struct {
		DataType   string `gorm:"column:data_type"`
		IsNullable string `gorm:"column:is_nullable"`
	}
	if err := db.Raw(`
		SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'worker_release_revisions'
		  AND column_name = 'gvisor_compatible_providers'
	`).Scan(&releaseCompatibilityColumn).Error; err != nil {
		t.Fatal(err)
	}
	if releaseCompatibilityColumn.DataType != "jsonb" || releaseCompatibilityColumn.IsNullable != "NO" {
		t.Fatalf("Worker Release gVisor compatibility column = %#v", releaseCompatibilityColumn)
	}
	var releaseCompatibilityConstraint int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM pg_constraint
		WHERE connamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())
		  AND conname = 'chk_worker_release_gvisor_compatible_providers'
	`).Scan(&releaseCompatibilityConstraint).Error; err != nil {
		t.Fatal(err)
	}
	if releaseCompatibilityConstraint != 1 {
		t.Fatalf("Worker Release gVisor compatibility constraint count = %d", releaseCompatibilityConstraint)
	}
	var duplicateCompatibilityAccepted bool
	if err := db.Raw(`SELECT synara_jsonb_text_array_is_unique('["codex", "codex"]'::jsonb)`).
		Scan(&duplicateCompatibilityAccepted).Error; err != nil {
		t.Fatal(err)
	}
	if duplicateCompatibilityAccepted {
		t.Fatal("PostgreSQL Worker Release gVisor compatibility accepted duplicate Providers")
	}
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platformConfig.Profile, "runtime-isolation-postgres-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID, turnID, executionID, targetID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Runtime isolation PostgreSQL", Visibility: "organization", DefaultBranch: "main", CreatedBy: domain.UserID},
		&persistence.ExecutionTarget{ID: targetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "gVisor PostgreSQL", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Runtime isolation PostgreSQL", Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: targetID},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "queued", InputText: "verify PostgreSQL runtime isolation"},
		&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Status: "queued", Attempt: 1, ExecutionTargetID: targetID, TargetKind: "kubernetes", RequestedBy: domain.UserID, QueuedAt: now},
		&persistence.ExecutionGenerationFact{TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, ExecutionTargetID: targetID, TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim", WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	runtimeName, profile, runtimeClass := "gvisor", string(platform.IsolationGVisorSandboxed), "synara-gvisor"
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	expiresAt := now.Add(time.Minute)
	decision := persistence.ExecutionRuntimeIsolationDecision{
		TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, ExecutionTargetID: targetID,
		AllocationBackend: "native-pod", RequestedRuntime: runtimeName, RequestedProfile: profile,
		EffectiveRuntime: &runtimeName, EffectiveProfile: &profile, PolicySource: "target-explicit", Decision: "selected",
		RuntimeClassName: &runtimeClass, AttestationDigest: &digest, AttestedAt: &now, AttestationExpiresAt: &expiresAt, CreatedAt: now,
	}
	if err := db.Create(&decision).Error; err != nil {
		t.Fatalf("insert PostgreSQL runtime isolation decision: %v", err)
	}
	if err := db.Model(&decision).Update("decision", "fallback").Error; err == nil {
		t.Fatal("PostgreSQL runtime isolation decision was mutable")
	}
	if err := db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Updates(map[string]any{"status": "completed", "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	dockerTargetID, dockerSessionID, dockerTurnID, dockerExecutionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	dockerModels := []any{
		&persistence.ExecutionTarget{
			ID: dockerTargetID, TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
			Kind: "docker", Name: "Docker runsc PostgreSQL", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
		},
		&persistence.AgentSession{
			ID: dockerSessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Docker runtime isolation PostgreSQL",
			Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: dockerTargetID,
		},
		&persistence.AgentTurn{
			ID: dockerTurnID, TenantID: domain.TenantID, SessionID: dockerSessionID,
			CreatedBy: domain.UserID, Status: "queued", InputText: "verify Docker runtime isolation",
		},
		&persistence.AgentExecution{
			ID: dockerExecutionID, TenantID: domain.TenantID, SessionID: dockerSessionID, TurnID: dockerTurnID,
			Status: "queued", Attempt: 1, ExecutionTargetID: dockerTargetID,
			TargetKind: "docker", RequestedBy: domain.UserID, QueuedAt: now,
		},
		&persistence.ExecutionGenerationFact{
			TenantID: domain.TenantID, ExecutionID: dockerExecutionID, Generation: 1,
			SessionID: dockerSessionID, TurnID: dockerTurnID, ExecutionTargetID: dockerTargetID,
			TargetKind: "docker", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
			DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range dockerModels {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed Docker %T: %v", model, err)
		}
	}
	dockerRuntime, dockerProfile := "gvisor", string(platform.IsolationSingleTenantTrusted)
	dockerDecision := persistence.ExecutionRuntimeIsolationDecision{
		TenantID: domain.TenantID, ExecutionID: dockerExecutionID, Generation: 1,
		ExecutionTargetID: dockerTargetID, AllocationBackend: "docker-engine",
		RequestedRuntime: "auto", RequestedProfile: dockerProfile,
		EffectiveRuntime: &dockerRuntime, EffectiveProfile: &dockerProfile,
		PolicySource: "target-auto", Decision: "selected", CreatedAt: now,
	}
	if err := db.Create(&dockerDecision).Error; err != nil {
		t.Fatalf("insert PostgreSQL trusted Docker gVisor decision: %v", err)
	}
	observation := persistence.ExecutionTargetRuntimeIsolationObservation{
		ExecutionTargetID: targetID, DetectedRuntimes: []string{"gvisor", "runc"},
		DetectedProfiles: []string{"kubernetes-restricted-v1", "gvisor-sandboxed-v1"},
		RequestedRuntime: runtimeName, RequestedProfile: profile,
		EffectiveRuntime: &runtimeName, EffectiveProfile: &profile,
		PolicySource: "target-explicit", Decision: "selected", State: "available",
		ObservedAt: now, ExpiresAt: expiresAt, UpdatedAt: now,
	}
	if err := db.Create(&observation).Error; err != nil {
		t.Fatalf("insert PostgreSQL runtime isolation observation: %v", err)
	}
	if err := db.Model(&observation).Updates(map[string]any{
		"state": "stale", "reason_code": "gvisor_attestation_stale",
		"observed_at": now.Add(time.Second), "expires_at": expiresAt.Add(time.Second), "updated_at": now.Add(time.Second),
	}).Error; err != nil {
		t.Fatalf("update PostgreSQL runtime isolation observation: %v", err)
	}
}
