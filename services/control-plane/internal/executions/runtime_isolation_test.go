package executions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestListRuntimeIsolationDecisionsProjectsBoundedGenerationFacts(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	now := time.Now().UTC()
	turnID := uuid.New()
	executionID := uuid.New()
	runtimeName, profile := "gvisor", "gvisor-sandboxed-v1"
	runtimeClass := "synara-gvisor"
	digest := strings.Repeat("a", 64)
	expiresAt := now.Add(time.Minute)
	models := []any{
		&persistence.AgentTurn{
			ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
			CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "runtime isolation",
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
			Status: "queued", Attempt: 1, ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes",
			RequestedBy: fixture.principal.UserID, QueuedAt: now,
		},
		&persistence.ExecutionGenerationFact{
			TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
			SessionID: fixture.sessionID, TurnID: turnID, ExecutionTargetID: fixture.targetID,
			TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
			DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ExecutionRuntimeIsolationDecision{
			TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
			ExecutionTargetID: fixture.targetID, AllocationBackend: "native-pod",
			RequestedRuntime: "gvisor", RequestedProfile: profile,
			EffectiveRuntime: &runtimeName, EffectiveProfile: &profile,
			PolicySource: "target-explicit", Decision: "selected", RuntimeClassName: &runtimeClass,
			AttestationDigest: &digest, AttestedAt: &now, AttestationExpiresAt: &expiresAt, CreatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}

	items, err := fixture.service.ListRuntimeIsolationDecisions(
		context.Background(), fixture.principal, executionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Generation != 1 || items[0].EffectiveRuntime == nil ||
		*items[0].EffectiveRuntime != runtimeName || items[0].EffectiveProfile == nil ||
		*items[0].EffectiveProfile != profile {
		t.Fatalf("runtime isolation decisions = %#v", items)
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), digest) || strings.Contains(string(encoded), "attestationDigest") {
		t.Fatalf("runtime isolation API leaked the attestation digest: %s", encoded)
	}
}

func TestDockerClaimBindsFreshTargetRuntimeDecisionToGeneration(t *testing.T) {
	fixture := newAdvancedOperationFixture(t, nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	targetID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
			Kind: "docker", Name: "Docker runsc", Status: "active", Capabilities: map[string]any{},
			CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Second),
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
			CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "bind Docker runtime",
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
			Status: "queued", Attempt: 1, Generation: 1, ExecutionTargetID: targetID,
			TargetKind: "docker", RequestedBy: fixture.principal.UserID, QueuedAt: now,
		},
		&persistence.ExecutionGenerationFact{
			TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
			SessionID: fixture.sessionID, TurnID: turnID, ExecutionTargetID: targetID,
			TargetKind: "docker", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
			DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	effectiveRuntime, profile := "gvisor", "single-tenant-trusted-v1"
	observation := persistence.ExecutionTargetRuntimeIsolationObservation{
		ExecutionTargetID: targetID, DetectedRuntimes: []string{"gvisor", "runc"},
		DetectedProfiles: []string{profile}, RequestedRuntime: "auto", RequestedProfile: profile,
		EffectiveRuntime: &effectiveRuntime, EffectiveProfile: &profile,
		PolicySource: "target-auto", Decision: "selected", State: "available",
		ObservedAt: now, ExpiresAt: now.Add(time.Minute), UpdatedAt: now,
	}
	if err := fixture.db.Create(&observation).Error; err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.First(&execution, "id = ?", executionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		return fixture.service.bindDockerRuntimeIsolationDecisionLocked(
			context.Background(), tx, execution, now.Add(time.Second),
		)
	}); err != nil {
		t.Fatal(err)
	}
	var decision persistence.ExecutionRuntimeIsolationDecision
	if err := fixture.db.First(
		&decision, "tenant_id = ? AND execution_id = ? AND generation = ?",
		fixture.tenantID, executionID, 1,
	).Error; err != nil {
		t.Fatal(err)
	}
	if decision.AllocationBackend != "docker-engine" || decision.EffectiveRuntime == nil ||
		*decision.EffectiveRuntime != "gvisor" || decision.EffectiveProfile == nil ||
		*decision.EffectiveProfile != profile || decision.AttestationDigest != nil || decision.RuntimeClassName != nil {
		t.Fatalf("Docker Generation runtime decision = %#v", decision)
	}
	observation.ExpiresAt = now
	observation.UpdatedAt = now.Add(2 * time.Second)
	if err := fixture.db.Save(&observation).Error; err != nil {
		t.Fatal(err)
	}
	execution.Generation = 2
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", executionID).Update("generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	generation := persistence.ExecutionGenerationFact{
		TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 2,
		SessionID: fixture.sessionID, TurnID: turnID, ExecutionTargetID: targetID,
		TargetKind: "docker", Provider: "codex", RecoveryReason: "execution-recovery",
		WarmPoolMode: "disabled", WarmPoolResult: "not-requested",
		DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&generation).Error; err != nil {
		t.Fatal(err)
	}
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		return fixture.service.bindDockerRuntimeIsolationDecisionLocked(
			context.Background(), tx, execution, now.Add(3*time.Second),
		)
	})
	assertProblemCode(t, err, "runtime_isolation_observation_stale")
}
