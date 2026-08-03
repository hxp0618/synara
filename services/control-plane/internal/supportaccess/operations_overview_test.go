package supportaccess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformTenantOverviewProjectsOperationalInventory(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "platform-ops-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	customer, err := tenancy.NewService(store.DB()).CreateTenant(
		ctx,
		identity.Principal{UserID: domain.UserID},
		tenancy.CreateTenantInput{
			Slug: "operations-" + uuid.NewString()[:8], Name: "Operations Customer",
			Region: "us-east-1", PlanCode: "enterprise", Status: "active",
		},
		"create-operations-customer",
		"127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	organizationID, targetID := uuid.New(), uuid.New()
	projectID, sessionID := uuid.New(), uuid.New()
	queuedTurnID, queuedExecutionID := uuid.New(), uuid.New()
	failedTurnID, failedExecutionID := uuid.New(), uuid.New()
	targetTenantID := customer.ID
	failureCode := "provider_failed"
	finishedAt := now.Add(-time.Hour)
	models := []any{
		&persistence.Organization{
			ID: organizationID, TenantID: customer.ID, Slug: "engineering", Name: "Engineering",
			Kind: "team", Status: "active", Settings: map[string]any{}, CreatedBy: domain.UserID,
		},
		&persistence.ExecutionTarget{
			ID: targetID, TenantID: &targetTenantID, Kind: "local", Name: "Customer Workers",
			Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
		},
		&persistence.WorkerInstance{
			ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
			TargetKind: "local", WorkerMode: "general-pool", ClusterID: "local", Namespace: "default",
			PodName: "worker-offline", Version: "test", ProtocolVersion: 1, Capabilities: map[string]any{},
			AuthTokenHash: bytes.Repeat([]byte{0x31}, 32), Status: "offline", AdministrativeStatus: "active",
			RegisteredAt: now.Add(-time.Hour), LastHeartbeatAt: now.Add(-10 * time.Minute),
		},
		&persistence.Project{
			ID: projectID, TenantID: customer.ID, OrganizationID: organizationID, Name: "Operations",
			DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: customer.ID, OrganizationID: organizationID, ProjectID: projectID,
			CreatedBy: domain.UserID, Title: "Operations Session", Status: "active", Visibility: "organization",
			Provider: "codex", ExecutionTargetID: targetID,
		},
		&persistence.AgentTurn{
			ID: queuedTurnID, TenantID: customer.ID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "queued", InputText: "queued", CreatedAt: now.Add(-75 * time.Minute),
		},
		&persistence.AgentExecution{
			ID: queuedExecutionID, TenantID: customer.ID, SessionID: sessionID, TurnID: queuedTurnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "local",
			RequestedBy: domain.UserID, QueuedAt: now.Add(-75 * time.Minute),
		},
		&persistence.AgentTurn{
			ID: failedTurnID, TenantID: customer.ID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "failed", InputText: "failed", CreatedAt: now.Add(-2 * time.Hour), CompletedAt: &finishedAt,
		},
		&persistence.AgentExecution{
			ID: failedExecutionID, TenantID: customer.ID, SessionID: sessionID, TurnID: failedTurnID,
			Attempt: 1, Status: "failed", ExecutionTargetID: targetID, TargetKind: "local",
			RequestedBy: domain.UserID, QueuedAt: now.Add(-2 * time.Hour), FinishedAt: &finishedAt,
			FailureCode: &failureCode,
		},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed operations overview %T: %v", model, err)
		}
	}

	artifactSize := int64(1536)
	artifactSHA := sha256.Sum256([]byte("platform operations artifact"))
	artifactDigest := hex.EncodeToString(artifactSHA[:])
	contentType := "text/plain"
	uploadExpiresAt := now.Add(time.Hour)
	artifacts := []persistence.Artifact{
		{
			ID: uuid.New(), TenantID: customer.ID, OrganizationID: organizationID, ProjectID: projectID,
			SessionID: sessionID, Kind: "generated_file", Status: "ready", Bucket: "test",
			ObjectKey: customer.ID.String() + "/ready.txt", ContentType: &contentType, SizeBytes: &artifactSize,
			SHA256: &artifactDigest, CreatedByType: "user", CreatedByID: domain.UserID, ReadyAt: &now,
		},
		{
			ID: uuid.New(), TenantID: customer.ID, OrganizationID: organizationID, ProjectID: projectID,
			SessionID: sessionID, Kind: "generated_file", Status: "pending", Bucket: "test",
			ObjectKey: customer.ID.String() + "/pending.txt", CreatedByType: "user", CreatedByID: domain.UserID,
			UploadTokenHash: bytes.Repeat([]byte{0x32}, 32), UploadExpiresAt: &uploadExpiresAt,
		},
	}
	for index := range artifacts {
		if err := store.DB().Create(&artifacts[index]).Error; err != nil {
			t.Fatalf("seed operations Artifact: %v", err)
		}
	}

	revokedAt := now.Add(-time.Hour)
	credentials := []persistence.ProviderCredential{
		operationsCredential(customer.ID, organizationID, domain.UserID, "Active Credential", nil),
		operationsCredential(customer.ID, organizationID, domain.UserID, "Revoked Credential", &revokedAt),
	}
	for index := range credentials {
		if err := store.DB().Create(&credentials[index]).Error; err != nil {
			t.Fatalf("seed operations Credential: %v", err)
		}
	}
	clientID := "operations-client"
	for _, status := range []string{"active", "disabled"} {
		connection := persistence.IdentityConnection{
			ID: uuid.New(), TenantID: customer.ID, Kind: "oidc", Name: strings.ToUpper(status) + " SSO",
			Status: status, Issuer: "https://" + status + ".idp.example.com", ClientID: &clientID,
			Configuration: map[string]any{}, CreatedBy: domain.UserID, UpdatedBy: domain.UserID,
		}
		if err := store.DB().Create(&connection).Error; err != nil {
			t.Fatalf("seed operations Identity Connection: %v", err)
		}
	}

	service := NewService(store.DB(), domain.TenantID)
	service.now = func() time.Time { return now }
	operatorTenantID := domain.TenantID
	overview, err := service.ListPlatformTenants(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &operatorTenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if overview.OperatorRole != "owner" || !overview.GeneratedAt.Equal(now) || len(overview.Items) != 1 {
		t.Fatalf("unexpected Platform overview envelope: %#v", overview)
	}
	item := overview.Items[0]
	// CreateTenant provisions one default Organization; the explicit Engineering
	// Organization above proves the aggregate includes both.
	if item.ID != customer.ID || item.OrganizationCount != 2 || item.SessionCount != 1 ||
		item.ExecutionTargetCount != 1 || item.WorkerCount != 1 || item.OfflineWorkerCount != 1 ||
		item.ActiveExecutionCount != 1 || item.QueuedExecutionCount != 1 || item.OldestQueuedAt == nil ||
		!item.OldestQueuedAt.Equal(now.Add(-75*time.Minute)) || item.FailedExecutionCount24h != 1 ||
		item.ArtifactCount != 2 || item.ArtifactBytes != artifactSize || item.PendingArtifactCount != 1 ||
		item.ActiveCredentialCount != 1 || item.UnavailableCredentialCount != 1 ||
		item.ActiveIdentityConnectionCount != 1 || item.DisabledIdentityConnectionCount != 1 {
		t.Fatalf("unexpected Platform operations inventory: %#v", item)
	}
}

func operationsCredential(
	tenantID, organizationID, actorID uuid.UUID,
	name string,
	revokedAt *time.Time,
) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: &organizationID, Scope: "organization",
		Name: name, Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: bytes.Repeat([]byte{0x61}, 32), EncryptedDataKey: bytes.Repeat([]byte{0x62}, 32),
		KMSProvider: "local", KMSKeyID: "test", AADVersion: 3, Version: 1,
		CreatedBy: actorID, UpdatedBy: actorID, RevokedAt: revokedAt,
	}
}
