package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestPolarisTypeScriptSDKAgainstRealControlPlane(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is required for TypeScript SDK conformance")
	}
	runPolarisSDKConformance(t, "TypeScript", bun, "packages/polaris-sdk/conformance/control-plane.ts")
}

func TestPolarisPythonSDKAgainstRealControlPlane(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for Python SDK conformance")
	}
	runPolarisSDKConformance(t, "Python", python, "packages/polaris-python/conformance/control_plane.py")
}

func runPolarisSDKConformance(t *testing.T, sdkName, runtime, relativeScriptPath string) {
	t.Helper()
	if testing.Short() {
		t.Skip("cross-process SDK conformance is disabled in short mode")
	}
	fixture := newProviderCapabilityHTTPFixture(t)
	approvalRequestID := "sdk-conformance-approval-" + uuid.NewString()
	userInputRequestID := seedSDKConformanceInteractions(t, fixture, approvalRequestID)
	artifactID := seedSDKConformanceArtifact(t, fixture)
	issued := issueDeveloperAPIKey(t, fixture, "agent_operator", []string{"api.access"})
	targetManager := issueTenantDeveloperAPIKey(t, fixture, "owner")

	httpServer := httptest.NewServer(fixture.handler)
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	repositoryRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(repositoryRoot, filepath.FromSlash(relativeScriptPath))
	commandContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	provisioningCompleted := make(chan error, 1)
	go func() { provisioningCompleted <- completeSDKConformanceProvisioning(commandContext, fixture.db) }()
	command := exec.CommandContext(commandContext, runtime, scriptPath)
	command.Dir = repositoryRoot
	command.Env = append(os.Environ(),
		"POLARIS_CONFORMANCE_BASE_URL="+httpServer.URL,
		"POLARIS_CONFORMANCE_API_KEY="+issued.Token,
		"POLARIS_CONFORMANCE_PROJECT_ID="+fixture.projectID.String(),
		"POLARIS_CONFORMANCE_EXECUTION_TARGET_ID="+fixture.targetID.String(),
		"POLARIS_CONFORMANCE_APPROVAL_EXECUTION_ID="+fixture.executionID.String(),
		"POLARIS_CONFORMANCE_APPROVAL_SESSION_ID="+fixture.sessionID.String(),
		"POLARIS_CONFORMANCE_APPROVAL_REQUEST_ID="+approvalRequestID,
		"POLARIS_CONFORMANCE_USER_INPUT_REQUEST_ID="+userInputRequestID,
		"POLARIS_CONFORMANCE_ARTIFACT_ID="+artifactID.String(),
		"POLARIS_CONFORMANCE_TARGET_API_KEY="+targetManager.Token,
		"POLARIS_CONFORMANCE_TENANT_ID="+fixture.tenantID.String(),
		"POLARIS_CONFORMANCE_ORGANIZATION_ID="+fixture.organizationID.String(),
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if err != nil {
		t.Fatalf("%s SDK conformance failed: %v\n%s", sdkName, err, stderr.String())
	}
	var result struct {
		ProjectID               uuid.UUID `json:"projectId"`
		ArchivedProjectID       uuid.UUID `json:"archivedProjectId"`
		ArchivedSessionID       uuid.UUID `json:"archivedSessionId"`
		CancelledExecutionID    uuid.UUID `json:"cancelledExecutionId"`
		InterruptedExecutionID  uuid.UUID `json:"interruptedExecutionId"`
		RollbackEventID         uuid.UUID `json:"rollbackEventId"`
		ForkedSessionID         uuid.UUID `json:"forkedSessionId"`
		SessionID               uuid.UUID `json:"sessionId"`
		TurnID                  uuid.UUID `json:"turnId"`
		EventTypes              []string  `json:"eventTypes"`
		ApprovalID              uuid.UUID `json:"approvalId"`
		TargetID                uuid.UUID `json:"targetId"`
		ProvisioningOperationID uuid.UUID `json:"provisioningOperationId"`
		CreatedArtifactID       uuid.UUID `json:"createdArtifactId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode %s SDK conformance output %q: %v", sdkName, stdout.String(), err)
	}
	if result.ProjectID == uuid.Nil || result.ArchivedProjectID == uuid.Nil || result.ArchivedSessionID == uuid.Nil || result.CancelledExecutionID == uuid.Nil || result.InterruptedExecutionID == uuid.Nil || result.RollbackEventID == uuid.Nil || result.ForkedSessionID == uuid.Nil || result.SessionID == uuid.Nil || result.TurnID == uuid.Nil || result.ApprovalID == uuid.Nil || result.TargetID == uuid.Nil || result.ProvisioningOperationID == uuid.Nil || result.CreatedArtifactID == uuid.Nil {
		t.Fatalf("incomplete %s SDK conformance result: %#v", sdkName, result)
	}
	if err := <-provisioningCompleted; err != nil {
		t.Fatalf("complete %s provisioning conformance: %v", sdkName, err)
	}
	if len(result.EventTypes) < 2 {
		t.Fatalf("%s SDK did not observe the expected event replay: %#v", sdkName, result.EventTypes)
	}

	var interaction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.tenantID, fixture.executionID, approvalRequestID,
	).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.Status != "resolved" || interaction.ResolvedBy == nil || *interaction.ResolvedBy != fixture.ownerUserID {
		t.Fatalf("machine approval responsibility projection = %#v", interaction)
	}
	var auditLog persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		fixture.tenantID, "execution.approval_resolved", interaction.ID,
	).Take(&auditLog).Error; err != nil {
		t.Fatal(err)
	}
	if auditLog.ActorType != "service_account" || auditLog.ActorID == nil || *auditLog.ActorID != issued.Account.ID {
		t.Fatalf("machine approval Audit attribution = %#v", auditLog)
	}
	var userInputInteraction persistence.ExecutionInteraction
	if err := fixture.db.Where(
		"tenant_id = ? AND execution_id = ? AND request_id = ?",
		fixture.tenantID, fixture.executionID, userInputRequestID,
	).Take(&userInputInteraction).Error; err != nil {
		t.Fatal(err)
	}
	if userInputInteraction.Status != "resolved" {
		t.Fatalf("machine user-input projection = %#v", userInputInteraction)
	}
	var userInputAudit persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		fixture.tenantID, "execution.user-input_resolved", userInputInteraction.ID,
	).Take(&userInputAudit).Error; err != nil {
		t.Fatal(err)
	}
	if userInputAudit.ActorType != "service_account" || userInputAudit.ActorID == nil || *userInputAudit.ActorID != issued.Account.ID {
		t.Fatalf("machine user-input Audit attribution = %#v", userInputAudit)
	}
	var targetAudit persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		fixture.tenantID, "execution_target.created", result.TargetID,
	).Take(&targetAudit).Error; err != nil {
		t.Fatal(err)
	}
	if targetAudit.ActorType != "service_account" || targetAudit.ActorID == nil || *targetAudit.ActorID != targetManager.Account.ID {
		t.Fatalf("machine Execution Target Audit attribution = %#v", targetAudit)
	}
	var projectAudit persistence.AuditLog
	if err := fixture.db.Where(
		"tenant_id = ? AND action = ? AND resource_id = ?",
		fixture.tenantID, "project.created", result.ProjectID,
	).Take(&projectAudit).Error; err != nil {
		t.Fatal(err)
	}
	if projectAudit.ActorType != "service_account" || projectAudit.ActorID == nil || *projectAudit.ActorID != targetManager.Account.ID {
		t.Fatalf("machine Project Audit attribution = %#v", projectAudit)
	}
	var projectReceipt persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND operation = ?",
		fixture.tenantID, targetManager.Account.ID, "project.create",
	).Take(&projectReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if projectReceipt.CompletedAt == nil {
		t.Fatalf("machine Project idempotency receipt is incomplete: %#v", projectReceipt)
	}
	for _, mutation := range []struct {
		operation  string
		action     string
		resourceID uuid.UUID
	}{
		{operation: "project.update", action: "project.updated", resourceID: result.ProjectID},
		{operation: "project.archive", action: "project.archived", resourceID: result.ArchivedProjectID},
	} {
		var receipt persistence.APIIdempotencyKey
		if err := fixture.db.Where(
			"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, targetManager.Account.ID, mutation.operation,
		).Take(&receipt).Error; err != nil {
			t.Fatal(err)
		}
		if receipt.CompletedAt == nil {
			t.Fatalf("machine %s idempotency receipt is incomplete: %#v", mutation.operation, receipt)
		}
		var auditCount int64
		if err := fixture.db.Model(&persistence.AuditLog{}).Where(
			"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
			fixture.tenantID, mutation.action, mutation.resourceID, "service_account", targetManager.Account.ID,
		).Count(&auditCount).Error; err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Fatalf("machine %s audit count = %d", mutation.action, auditCount)
		}
	}
	var archivedProject persistence.Project
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.ArchivedProjectID).Take(&archivedProject).Error; err != nil {
		t.Fatal(err)
	}
	if archivedProject.ArchivedAt == nil {
		t.Fatalf("machine archived Project remained active: %#v", archivedProject)
	}
	var archivedSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.ArchivedSessionID).Take(&archivedSession).Error; err != nil {
		t.Fatal(err)
	}
	if archivedSession.Status != "archived" || archivedSession.ArchivedAt == nil {
		t.Fatalf("machine archived Session remained active: %#v", archivedSession)
	}
	var sessionArchiveAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
		fixture.tenantID, "session.archived", result.ArchivedSessionID, "service_account", issued.Account.ID,
	).Count(&sessionArchiveAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if sessionArchiveAuditCount != 1 {
		t.Fatalf("machine Session archive audit count = %d", sessionArchiveAuditCount)
	}
	var sessionArchiveReceipt persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, "session.archive",
	).Take(&sessionArchiveReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if sessionArchiveReceipt.CompletedAt == nil {
		t.Fatalf("machine Session archive idempotency receipt is incomplete: %#v", sessionArchiveReceipt)
	}
	var sessionModelSwitchReceipt persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, "session.model.switch",
	).Take(&sessionModelSwitchReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if sessionModelSwitchReceipt.CompletedAt == nil {
		t.Fatalf("machine Session model-switch idempotency receipt is incomplete: %#v", sessionModelSwitchReceipt)
	}
	for _, transition := range []struct {
		operation string
		action    string
	}{
		{operation: "session.suspend", action: "session.suspended"},
		{operation: "session.resume", action: "session.resumed"},
	} {
		var receipt persistence.APIIdempotencyKey
		if err := fixture.db.Where(
			"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, transition.operation,
		).Take(&receipt).Error; err != nil {
			t.Fatal(err)
		}
		if receipt.CompletedAt == nil {
			t.Fatalf("machine %s idempotency receipt is incomplete: %#v", transition.operation, receipt)
		}
		var auditCount int64
		if err := fixture.db.Model(&persistence.AuditLog{}).Where(
			"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
			fixture.tenantID, transition.action, result.ArchivedSessionID, "service_account", issued.Account.ID,
		).Count(&auditCount).Error; err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Fatalf("machine %s audit count = %d", transition.action, auditCount)
		}
	}
	var cancelledExecution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.CancelledExecutionID).Take(&cancelledExecution).Error; err != nil {
		t.Fatal(err)
	}
	if cancelledExecution.Status != "cancelled" || cancelledExecution.FinishedAt == nil || cancelledExecution.WorkerID != nil {
		t.Fatalf("machine cancelled Execution projection = %#v", cancelledExecution)
	}
	var cancelAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
		fixture.tenantID, "execution.cancelled", result.CancelledExecutionID, "service_account", issued.Account.ID,
	).Count(&cancelAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if cancelAuditCount != 1 {
		t.Fatalf("machine Execution cancel audit count = %d", cancelAuditCount)
	}
	var cancelReceipt persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, "execution.cancel",
	).Take(&cancelReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if cancelReceipt.CompletedAt == nil {
		t.Fatalf("machine Execution cancel idempotency receipt is incomplete: %#v", cancelReceipt)
	}
	var interruptedExecution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.InterruptedExecutionID).Take(&interruptedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if interruptedExecution.Status != "cancelled" || interruptedExecution.FinishedAt == nil {
		t.Fatalf("machine interrupted queued Execution projection = %#v", interruptedExecution)
	}
	var interruptAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
		fixture.tenantID, "turn.interrupt_requested", result.InterruptedExecutionID, "service_account", issued.Account.ID,
	).Count(&interruptAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if interruptAuditCount != 1 {
		t.Fatalf("machine Turn interrupt audit count = %d", interruptAuditCount)
	}
	var interruptReceipt persistence.APIIdempotencyKey
	if err := fixture.db.Where(
		"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, "session.turn.interrupt",
	).Take(&interruptReceipt).Error; err != nil {
		t.Fatal(err)
	}
	if interruptReceipt.CompletedAt == nil {
		t.Fatalf("machine Turn interrupt idempotency receipt is incomplete: %#v", interruptReceipt)
	}
	var rollbackEvent persistence.SessionEvent
	if err := fixture.db.Where(
		"tenant_id = ? AND event_id = ? AND event_type = ?", fixture.tenantID,
		result.RollbackEventID, "session.history.rolled-back",
	).Take(&rollbackEvent).Error; err != nil {
		t.Fatal(err)
	}
	var forkedSession persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.ForkedSessionID).Take(&forkedSession).Error; err != nil {
		t.Fatal(err)
	}
	if forkedSession.ForkSourceSessionID == nil || forkedSession.Visibility != "organization" {
		t.Fatalf("machine Forked Session projection = %#v", forkedSession)
	}
	for _, operation := range []struct {
		name       string
		action     string
		resourceID uuid.UUID
	}{
		{name: "session.rollback", action: "session.history_rolled_back", resourceID: rollbackEvent.SessionID},
		{name: "session.fork", action: "session.forked", resourceID: result.ForkedSessionID},
	} {
		var receipt persistence.APIIdempotencyKey
		if err := fixture.db.Where(
			"tenant_id = ? AND actor_id = ? AND operation = ?", fixture.tenantID, issued.Account.ID, operation.name,
		).Take(&receipt).Error; err != nil {
			t.Fatal(err)
		}
		if receipt.CompletedAt == nil {
			t.Fatalf("machine %s receipt is incomplete: %#v", operation.name, receipt)
		}
		var auditCount int64
		if err := fixture.db.Model(&persistence.AuditLog{}).Where(
			"tenant_id = ? AND action = ? AND resource_id = ? AND actor_type = ? AND actor_id = ?",
			fixture.tenantID, operation.action, operation.resourceID, "service_account", issued.Account.ID,
		).Count(&auditCount).Error; err != nil {
			t.Fatal(err)
		}
		if auditCount != 1 {
			t.Fatalf("machine %s audit count = %d", operation.action, auditCount)
		}
	}
	var createdArtifact persistence.Artifact
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, result.CreatedArtifactID).Take(&createdArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if createdArtifact.Status != "deleted" || createdArtifact.DeletedAt == nil || createdArtifact.CreatedByType != "service_account" || createdArtifact.CreatedByID != issued.Account.ID {
		t.Fatalf("machine Artifact projection = %#v", createdArtifact)
	}
	var provisioning persistence.ExecutionTargetProvisioningOperation
	if err := fixture.db.Where("tenant_id = ? AND id = ? AND execution_target_id = ?", fixture.tenantID, result.ProvisioningOperationID, result.TargetID).Take(&provisioning).Error; err != nil {
		t.Fatal(err)
	}
	if provisioning.State != "succeeded" || provisioning.ActorType != "service_account" || provisioning.ActorID != targetManager.Account.ID {
		t.Fatalf("machine provisioning operation projection = %#v", provisioning)
	}
	var provisioningAuditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where(
		"tenant_id = ? AND resource_id = ? AND actor_type = ? AND actor_id = ? AND action IN ?",
		fixture.tenantID, result.ProvisioningOperationID, "service_account", targetManager.Account.ID,
		[]string{"execution_target.provisioning_accepted", "execution_target.provisioning_claimed", "execution_target.provisioning_succeeded"},
	).Count(&provisioningAuditCount).Error; err != nil {
		t.Fatal(err)
	}
	if provisioningAuditCount != 3 {
		t.Fatalf("machine provisioning Audit count = %d, want 3", provisioningAuditCount)
	}
}

func seedSDKConformanceArtifact(t *testing.T, fixture providerCapabilityHTTPFixture) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	artifactID := uuid.New()
	size := int64(17)
	digest := "c8a3a0fcd95fa0e978ea44ef6b54a46019ddc84d6a4b97b2c7ec78e61c56b55a"
	contentType := "text/plain"
	originalName := "conformance.txt"
	if err := fixture.db.Create(&persistence.Artifact{
		ID: artifactID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, SessionID: fixture.sessionID, ExecutionID: &fixture.executionID,
		Kind: "generated_file", Status: "ready", OriginalName: &originalName,
		Bucket: "sdk-conformance", ObjectKey: "sdk-conformance/" + artifactID.String(),
		ContentType: &contentType, SizeBytes: &size, SHA256: &digest,
		CreatedByType: "user", CreatedByID: fixture.ownerUserID, ReadyAt: &now, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return artifactID
}

func completeSDKConformanceProvisioning(ctx context.Context, db *gorm.DB) error {
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		return err
	}
	service := executiontargets.NewService(db, profile, nil)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		claim, found, err := service.ClaimNextProvisioningOperation(ctx, "sdk-conformance", time.Second)
		if err != nil {
			return err
		}
		if found {
			_, err = service.CompleteProvisioningSuccess(ctx, claim, executiontargets.SSHProvisionResult{
				TargetID: claim.Operation.TargetID, Operation: claim.Operation.Action, Status: "active",
			})
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func seedSDKConformanceInteractions(t *testing.T, fixture providerCapabilityHTTPFixture, requestID string) string {
	t.Helper()
	ctx := context.Background()
	registered, err := fixture.executionService.Register(ctx, executions.RegisterWorkerInput{
		ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", InstanceUID: uuid.NewString(),
		ClusterID: "sdk-conformance", Namespace: "tests", PodName: "sdk-conformance-" + uuid.NewString(),
		Version: "test", ProtocolVersion: executions.WorkerProtocolVersion,
		Capabilities: map[string]any{"providers": []any{"codex"}}, LeaseSupported: true, FencingSupported: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var worker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", registered.Worker.ID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Select("turn_id").Where(
		"tenant_id = ? AND id = ?", fixture.tenantID, fixture.executionID,
	).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	userInputRequestID := "sdk-conformance-user-input-" + uuid.NewString()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ? AND status = ?", fixture.tenantID, fixture.executionID, "queued").
			Updates(map[string]any{
				"status": "waiting-for-approval", "worker_id": worker.ID, "generation": 1, "started_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.WorkerLease{
			ExecutionID: fixture.executionID, TenantID: fixture.tenantID, WorkerID: worker.ID,
			WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID, Generation: 1,
			LeaseTokenHash: make([]byte, 32), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour),
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&persistence.ExecutionInteraction{
			ID: uuid.New(), TenantID: fixture.tenantID, ExecutionID: fixture.executionID,
			SessionID: fixture.sessionID, TurnID: execution.TurnID, WorkerID: worker.ID,
			Generation: 1, Provider: "codex", RequestID: requestID, EventVersion: executions.RuntimeEventVersionV2,
			Kind: "approval", Status: "pending",
			Payload: map[string]any{
				"requestId": requestID, "requestType": "command_execution_approval",
				"detail": "Approve the Stage 7 SDK conformance fixture.",
			},
			RequestedAt: now, ExpiresAt: now.Add(time.Hour), DeliveryStatus: "not-ready",
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.ExecutionInteraction{
			ID: uuid.New(), TenantID: fixture.tenantID, ExecutionID: fixture.executionID,
			SessionID: fixture.sessionID, TurnID: execution.TurnID, WorkerID: worker.ID,
			Generation: 1, Provider: "codex", RequestID: userInputRequestID, EventVersion: executions.RuntimeEventVersionV2,
			Kind: "user-input", Status: "pending",
			Payload: map[string]any{
				"requestId": userInputRequestID,
				"questions": []any{map[string]any{
					"id": "environment", "header": "Environment", "question": "Which environment?", "options": []any{},
				}},
			},
			RequestedAt: now.Add(time.Millisecond), ExpiresAt: now.Add(time.Hour), DeliveryStatus: "not-ready",
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return userInputRequestID
}
