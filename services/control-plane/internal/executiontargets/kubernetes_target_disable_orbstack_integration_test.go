package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDisableManagedKubernetesTargetOrbStackIntegration(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
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
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID := uuid.New(), uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "OrbStack Target disable",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Tenant{
				ID: tenantID, Slug: "orbstack-target-disable-" + uuid.NewString(), Name: "OrbStack Target disable",
				Status: "active", PlanCode: "test", Region: "local", Settings: map[string]any{},
				CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Organization{
				ID: organizationID, TenantID: tenantID, Slug: "root", Name: "OrbStack Target disable",
				Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID,
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
				Role: "owner", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x85}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targets := NewService(db, profile, cipher)
	principal := identity.Principal{UserID: userID, ActiveTenantID: &tenantID}
	target, err := targets.Create(ctx, principal, tenantID, CreateInput{
		OrganizationID: &organizationID,
		Kind:           "kubernetes",
		Name:           "orbstack-target-disable-" + uuid.NewString(),
		Configuration: map[string]any{
			"apiServer": apiServer, "bearerToken": bearerToken, "caCertificate": string(caCertificate),
			"namespace": namespace, "manageNamespace": false,
			"image": "busybox:1.36.1", "imagePullPolicy": "IfNotPresent",
			"controlPlaneUrl": "http://control-plane.invalid:3780", "allowInsecureControlPlane": true,
			"runnerCommand": []string{"true"}, "maxActivePods": 1,
			"egressCidrs": []string{"0.0.0.0/0"},
			"cpuRequest":  "10m", "cpuLimit": "100m", "pidsLimit": 512, "memoryRequest": "16Mi", "memoryLimit": "64Mi",
			"ephemeralStorageRequest": "32Mi", "ephemeralStorageLimit": "128Mi",
			"workspaceSizeLimit": "64Mi", "quotaCpuRequests": "500m", "quotaCpuLimits": "1",
			"quotaMemoryRequests": "256Mi", "quotaMemoryLimits": "512Mi",
		},
		Capabilities: map[string]any{"workspaceModes": []string{"local"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher := NewManagedKubernetesRoutingPublisher(targets, ManagedKubernetesRoutingPublisherConfig{
		PublisherIdentity: managedKubernetesRoutingPublisherPrefix + "target-disable-orbstack", ObservationTTL: time.Minute,
	})
	reconciler := NewKubernetesReconciler(targets, KubernetesReconcilerConfig{
		PublicControlPlaneURL: "http://control-plane.invalid:3780",
		PublishRoutingHealth:  publisher.PublishReconcile,
	}, slog.Default())
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var health persistence.ExecutionTargetHealth
	if err := db.Where("execution_target_id = ?", target.ID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if health.Status != routing.HealthHealthy || health.CapacityStatus != routing.CapacityAvailable ||
		health.AllocatedCapacityUnits != 0 || health.ReservationAcknowledgedUnits != 0 ||
		health.ReservationAuthorityMode == nil ||
		*health.ReservationAuthorityMode != routing.ReservationAuthorityExactActiveV1 {
		t.Fatalf("pre-disable OrbStack health = %#v", health)
	}
	var storedModel persistence.ExecutionTarget
	if err := db.Where("id = ?", target.ID).Take(&storedModel).Error; err != nil {
		t.Fatal(err)
	}
	configuration, err := reconciler.loadConfiguration(storedModel)
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
	if len(pods) != 0 {
		t.Fatalf("pre-disable OrbStack Pods = %#v", pods)
	}

	disabled, replayed, err := targets.DisableManagedKubernetesTarget(
		ctx, principal, tenantID, target.ID, "target-disable-orbstack", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || disabled.Status != "disabled" {
		t.Fatalf("OrbStack Target disable = %#v replayed=%t", disabled, replayed)
	}
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var healthAfter persistence.ExecutionTargetHealth
	if err := db.Where("execution_target_id = ?", target.ID).Take(&healthAfter).Error; err != nil {
		t.Fatal(err)
	}
	if healthAfter.Version != health.Version || !healthAfter.ObservedAt.Equal(health.ObservedAt) {
		t.Fatalf("disabled Target was reconciled again: before=%#v after=%#v", health, healthAfter)
	}
	pods, err = client.ListPods(ctx, namespace, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 0 {
		t.Fatalf("disabled OrbStack Target regained Pods: %#v", pods)
	}
	stored, err := targets.Get(ctx, principal, tenantID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "disabled" {
		t.Fatalf("stored disabled Target = %#v", stored)
	}
}
