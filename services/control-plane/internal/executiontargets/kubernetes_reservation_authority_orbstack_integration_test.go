package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestKubernetesReservationAuthorityOrbStackIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("SYNARA_TEST_DATABASE_URL"))
	apiServer := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_API_SERVER"))
	bearerToken := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_BEARER_TOKEN"))
	caData := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CA_DATA"))
	namespace := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NAMESPACE"))
	if databaseURL == "" || apiServer == "" || bearerToken == "" || caData == "" || namespace == "" {
		t.Skip("OrbStack Kubernetes/PostgreSQL acceptance environment is not configured")
	}
	caCertificate, err := base64.StdEncoding.DecodeString(caData)
	if err != nil || len(caCertificate) == 0 {
		t.Fatal("SYNARA_TEST_KUBERNETES_CA_DATA is not valid base64 CA data")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "orbstack-capacity-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x84}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targets := NewService(db, platformConfig, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := targets.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID,
		Kind:           "kubernetes",
		Name:           "orbstack-reservation-" + uuid.NewString(),
		Configuration: map[string]any{
			"apiServer": apiServer, "bearerToken": bearerToken, "caCertificate": string(caCertificate),
			"namespace": namespace, "manageNamespace": false,
			"image": "busybox:1.36.1", "imagePullPolicy": "IfNotPresent",
			"controlPlaneUrl": "http://control-plane.invalid:3780", "allowInsecureControlPlane": true,
			"runnerCommand": []string{"true"}, "maxActivePods": 1,
			"egressCidrs": []string{"0.0.0.0/0"},
			"cpuRequest":  "10m", "cpuLimit": "100m", "memoryRequest": "16Mi", "memoryLimit": "64Mi",
			"workspaceSizeLimit": "64Mi", "quotaCpuRequests": "500m", "quotaCpuLimits": "1",
			"quotaMemoryRequests": "256Mi", "quotaMemoryLimits": "512Mi",
		},
		Capabilities: map[string]any{"workspaceModes": []string{"local"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID := uuid.New()
	models := []any{&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "OrbStack reservation", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}}
	executionIDs := make([]uuid.UUID, 0, 2)
	for index := 0; index < 2; index++ {
		sessionID := uuid.New()
		turnID := uuid.New()
		executionID := uuid.New()
		executionIDs = append(executionIDs, executionID)
		models = append(models,
			&persistence.AgentSession{
				ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
				ProjectID: projectID, CreatedBy: domain.UserID, Title: fmt.Sprintf("OrbStack reservation %d", index),
				Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: target.ID,
				ResourceState: "provisioning", CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentTurn{
				ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
				Status: "queued", InputText: fmt.Sprintf("OrbStack reservation %d", index),
				TurnKind: "message", RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
			},
			&persistence.AgentExecution{
				ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
				Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
				Provider: orbstackStringPointer("codex"), Generation: 0, RequestedBy: domain.UserID,
				QueuedAt: now.Add(time.Duration(index) * time.Microsecond),
			},
		)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if createErr := tx.Create(model).Error; createErr != nil {
				return createErr
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	publisher := NewManagedKubernetesRoutingPublisher(targets, ManagedKubernetesRoutingPublisherConfig{
		PublisherIdentity: "managed-kubernetes-orbstack-reservation", ObservationTTL: time.Minute,
	})
	reconciler := NewKubernetesReconciler(targets, KubernetesReconcilerConfig{
		PublicControlPlaneURL:  "http://control-plane.invalid:3780",
		WorkerLeaseTTL:         30 * time.Second,
		WorkerHeartbeatTimeout: time.Minute,
		PublishRoutingHealth:   publisher.PublishReconcile,
	}, slog.Default())
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var health persistence.ExecutionTargetHealth
	if err := db.Where("execution_target_id = ?", target.ID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if health.ReservationAuthorityMode == nil ||
		*health.ReservationAuthorityMode != routing.ReservationAuthorityExactActiveV1 ||
		health.ReservationAcknowledgedUnits != 1 || health.ReservationAcknowledgementsSHA256 == nil ||
		health.AvailableCapacityUnits == nil || *health.AvailableCapacityUnits != 1 ||
		health.AllocatedCapacityUnits != 1 || health.Version != 2 {
		t.Fatalf("OrbStack reservation Health = %#v", health)
	}
	var acknowledgements []persistence.ExecutionTargetReservationAcknowledgement
	if err := db.Where("execution_target_id = ?", target.ID).Find(&acknowledgements).Error; err != nil {
		t.Fatal(err)
	}
	if len(acknowledgements) != 1 || acknowledgements[0].HealthVersion != health.Version ||
		acknowledgements[0].ExecutionGeneration != 0 ||
		(acknowledgements[0].ExecutionID != executionIDs[0] && acknowledgements[0].ExecutionID != executionIDs[1]) {
		t.Fatalf("OrbStack reservation acknowledgements = %#v", acknowledgements)
	}

	var storedTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", target.ID).Take(&storedTarget).Error; err != nil {
		t.Fatal(err)
	}
	configuration, err := reconciler.loadConfiguration(storedTarget)
	if err != nil {
		t.Fatal(err)
	}
	client, err := reconciler.factory.Open(configuration)
	if err != nil {
		t.Fatal(err)
	}
	pods, err := client.ListPods(ctx, namespace, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("OrbStack Target Pods = %#v", pods)
	}
}

func orbstackStringPointer(value string) *string { return &value }
