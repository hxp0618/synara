package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const orbStackKubernetesGuaranteedWarmIntegrationEnv = "SYNARA_ORBSTACK_KUBERNETES_WARM_TEST"

func TestKubernetesGuaranteedWarmOrbStackIntegration(t *testing.T) {
	if os.Getenv(orbStackKubernetesGuaranteedWarmIntegrationEnv) != "1" {
		t.Skip(orbStackKubernetesGuaranteedWarmIntegrationEnv + "=1 is required")
	}
	databaseURL := strings.TrimSpace(os.Getenv("SYNARA_TEST_DATABASE_URL"))
	apiServer := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_API_SERVER"))
	bearerToken := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_BEARER_TOKEN"))
	caData := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CA_DATA"))
	namespace := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NAMESPACE"))
	image := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_WORKER_IMAGE"))
	if databaseURL == "" || apiServer == "" || bearerToken == "" || caData == "" || namespace == "" || image == "" {
		t.Fatal("the OrbStack Kubernetes/PostgreSQL acceptance environment is incomplete")
	}
	caCertificate, err := base64.StdEncoding.DecodeString(caData)
	if err != nil || len(caCertificate) == 0 {
		t.Fatal("SYNARA_TEST_KUBERNETES_CA_DATA is not valid base64 CA data")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "orbstack-guaranteed-warm-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x93}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targets := NewService(db, profile, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}

	registrationBlocker := newOrbStackKubernetesRegistrationBlocker()
	registrationServer := httptest.NewServer(registrationBlocker)
	t.Cleanup(func() {
		registrationBlocker.Release()
		registrationServer.Close()
	})
	controlPlaneHost := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTROL_PLANE_HOST"))
	if controlPlaneHost == "" {
		controlPlaneHost = "host.docker.internal"
	}
	controlPlaneURL, err := orbStackKubernetesControlPlaneURL(registrationServer.URL, controlPlaneHost)
	if err != nil {
		t.Fatal(err)
	}

	target, err := targets.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID,
		Kind:           "kubernetes",
		Name:           "OrbStack guaranteed warm " + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		Configuration: map[string]any{
			"apiServer": apiServer, "bearerToken": bearerToken, "caCertificate": string(caCertificate),
			"namespace": namespace, "manageNamespace": false,
			"image": image, "imagePullPolicy": "Never",
			"controlPlaneUrl": controlPlaneURL, "allowInsecureControlPlane": true,
			"runnerCommand": []string{
				"/usr/local/bin/node",
				"/opt/synara/acceptance/provider-host-fixture.mjs",
			},
			"maxActivePods": 2, "egressCidrs": []string{"0.0.0.0/0"},
			"cpuRequest": "10m", "cpuLimit": "250m", "memoryRequest": "32Mi", "memoryLimit": "256Mi",
			"workspaceSizeLimit": "64Mi", "quotaCpuRequests": "500m", "quotaCpuLimits": "1",
			"quotaMemoryRequests": "512Mi", "quotaMemoryLimits": "1Gi",
		},
		Capabilities: map[string]any{
			"workspaceModes": []string{"local", "worktree"},
			"providerPolicy": map[string]any{"experimentalProviders": []string{"codex", "claudeAgent"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &domain.TenantID, ExecutionTargetID: target.ID,
		Name: "guaranteed-warm", Mode: placement.PoolModeWarm, CapacityClass: placement.CapacityClassInteractive,
		ClusterID: kubernetesLocalClusterID, Namespace: namespace,
		DesiredIdleUnits: 2, MinIdleUnits: 1, MaxActiveUnits: 2,
		SchedulingTemplate: map[string]any{}, Status: placement.PoolStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	placementPolicy := persistence.ExecutionPlacementPolicy{
		TenantID: &domain.TenantID, ExecutionTargetID: target.ID, Version: 1,
		DefaultPoolID: pool.ID, LowLatencyPoolID: &pool.ID,
		UpdatedBy: &domain.UserID, UpdatedAt: now,
	}
	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "OrbStack guaranteed warm", DefaultBranch: "main", Visibility: "organization",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: project.ID, CreatedBy: domain.UserID, Title: "OrbStack guaranteed warm",
		Status: "active", Visibility: "organization", Provider: "codex",
		ExecutionTargetID: target.ID, RequestedExecutionTargetID: target.ID,
		ResourceState: "idle", MeaningfulActivityAt: now, WarmPoolMode: placement.WarmPoolModeLowLatency,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range []any{&pool, &placementPolicy, &project, &session} {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	routingPublisher := NewManagedKubernetesRoutingPublisher(targets, ManagedKubernetesRoutingPublisherConfig{
		PublisherIdentity: managedKubernetesRoutingPublisherPrefix + "guaranteed-warm-orbstack",
		ObservationTTL:    time.Minute,
	})
	warmPublisher := NewManagedKubernetesWarmCapacityPublisher(targets, ManagedKubernetesWarmCapacityPublisherConfig{
		PublisherIdentity: managedKubernetesWarmCapacityPublisherPrefix + "guaranteed-warm-orbstack",
		ObservationTTL:    time.Minute,
	})
	reconciler := NewKubernetesReconciler(targets, KubernetesReconcilerConfig{
		PublicControlPlaneURL: controlPlaneURL,
		WorkerLeaseTTL:        12 * time.Second,
		PublishRoutingHealth:  routingPublisher.PublishReconcile,
		PublishWarmCapacity:   warmPublisher.PublishReconcile,
	}, slog.Default())
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
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		pods, listErr := client.ListPods(cleanupCtx, namespace, target.ID)
		if listErr != nil {
			t.Errorf("list exact acceptance Pods for cleanup: %v", listErr)
			return
		}
		for _, pod := range pods {
			if deleteErr := client.DeletePod(cleanupCtx, namespace, pod.Name, pod.UID); deleteErr != nil {
				t.Errorf("delete exact acceptance Pod %s/%s: %v", pod.Name, pod.UID, deleteErr)
			}
		}
	})

	startedAt := time.Now()
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	initialPods := waitForOrbStackKubernetesTargetPods(t, ctx, client, namespace, target.ID, "two running warm slots", func(pods []kubernetesPod) bool {
		if len(pods) != 2 {
			return false
		}
		seen := map[string]bool{}
		for _, pod := range pods {
			if pod.Phase != "Running" || pod.Labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeWarmPool {
				return false
			}
			seen[pod.Labels[kubernetesWarmSlotLabel]] = true
		}
		return seen["0"] && seen["1"]
	})
	waitForOrbStackKubernetesRegistrations(t, ctx, registrationBlocker, 2)
	initialBySlot := orbStackKubernetesWarmPodsBySlot(t, initialPods)
	initialAuthority := loadOrbStackKubernetesWarmAuthority(t, db, pool)
	assertOrbStackKubernetesWarmAuthority(t, initialAuthority, 1, 2, 0)

	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "exercise guaranteed warm capacity", CreatedAt: time.Now().UTC(),
	}
	provider := "codex"
	placementPolicyVersion := int64(1)
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, TurnID: turn.ID,
		Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: "kubernetes",
		WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, CapacityClass: &pool.CapacityClass,
		PlacementPolicyVersion: &placementPolicyVersion, PlacementClusterID: kubernetesLocalClusterID,
		WarmPoolModeSnapshot: placement.WarmPoolModeLowLatency,
		Provider:             &provider,
		RequestedBy:          domain.UserID, QueuedAt: time.Now().UTC(),
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatal(err)
	}
	decisionInput := schedulingdecision.NewSelectedOnlyInput(
		uuid.New(), schedulingdecision.AlgorithmFixedTargetV1, execution.QueuedAt,
		schedulingdecision.CandidateFromExecution(execution),
	)
	if _, err := schedulingdecision.CreateExecution(ctx, db, &execution, decisionInput); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	afterEviction := waitForOrbStackKubernetesTargetPods(t, ctx, client, namespace, target.ID, "only guaranteed warm slot after demand eviction", func(pods []kubernetesPod) bool {
		return len(pods) == 1 &&
			pods[0].Labels[kubernetesWorkerModeLabel] == kubernetesWorkerModeWarmPool &&
			pods[0].Labels[kubernetesWarmSlotLabel] == "0" &&
			pods[0].UID == initialBySlot["0"].UID
	})
	if afterEviction[0].UID != initialBySlot["0"].UID {
		t.Fatal("the guaranteed warm slot changed physical Pod UID")
	}
	if err := waitForOrbStackKubernetesPodUIDAbsent(ctx, client, namespace, target.ID, initialBySlot["1"].UID); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	finalPods := waitForOrbStackKubernetesTargetPods(t, ctx, client, namespace, target.ID, "guaranteed slot plus cold execution Pod", func(pods []kubernetesPod) bool {
		if len(pods) != 2 {
			return false
		}
		guaranteedFound, executionFound := false, false
		for _, pod := range pods {
			if pod.Labels[kubernetesWorkerModeLabel] == kubernetesWorkerModeWarmPool &&
				pod.Labels[kubernetesWarmSlotLabel] == "0" && pod.UID == initialBySlot["0"].UID {
				guaranteedFound = true
			}
			if pod.Labels[kubernetesWorkerModeLabel] == kubernetesWorkerModeExecutionPinned &&
				pod.Labels[kubernetesExecutionLabel] == execution.ID.String() {
				executionFound = true
			}
			if pod.Labels[kubernetesWarmSlotLabel] == "1" {
				return false
			}
		}
		return guaranteedFound && executionFound
	})
	finalAuthority := loadOrbStackKubernetesWarmAuthority(t, db, pool)
	assertOrbStackKubernetesWarmAuthority(t, finalAuthority, 1, 2, 0)
	if finalAuthority.Version <= initialAuthority.Version {
		t.Fatalf("warm authority version = %d, want > %d", finalAuthority.Version, initialAuthority.Version)
	}

	t.Logf(
		"OrbStack guaranteed-warm PASS namespace=%s target=%s pool=%s guaranteedUID=%s evictedUID=%s finalPods=%d authorityVersion=%d deficit=%d duration=%s registrationsBlocked=%d",
		namespace, target.ID, pool.ID, initialBySlot["0"].UID, initialBySlot["1"].UID, len(finalPods),
		finalAuthority.Version, finalAuthority.MinIdleUnits-finalAuthority.ReadyIdleUnits,
		time.Since(startedAt).Round(time.Millisecond), registrationBlocker.Count(),
	)
}

type orbStackKubernetesRegistrationBlocker struct {
	mu            sync.Mutex
	registrations int
	release       chan struct{}
	releaseOnce   sync.Once
}

func newOrbStackKubernetesRegistrationBlocker() *orbStackKubernetesRegistrationBlocker {
	return &orbStackKubernetesRegistrationBlocker{release: make(chan struct{})}
}

func (b *orbStackKubernetesRegistrationBlocker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/workers/register" {
		http.NotFound(writer, request)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
	b.mu.Lock()
	b.registrations++
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-request.Context().Done():
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_, _ = writer.Write([]byte(`{"error":{"code":"acceptance_registration_released","message":"acceptance completed"}}`))
}

func (b *orbStackKubernetesRegistrationBlocker) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registrations
}

func (b *orbStackKubernetesRegistrationBlocker) Release() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func orbStackKubernetesControlPlaneURL(serverURL, host string) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	port := parsed.Port()
	if port == "" || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("invalid acceptance Control Plane endpoint")
	}
	return "http://" + strings.TrimSpace(host) + ":" + port, nil
}

func waitForOrbStackKubernetesRegistrations(
	t *testing.T,
	ctx context.Context,
	blocker *orbStackKubernetesRegistrationBlocker,
	want int,
) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if blocker.Count() >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("blocked Kubernetes registrations = %d, want >= %d: %v", blocker.Count(), want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForOrbStackKubernetesTargetPods(
	t *testing.T,
	ctx context.Context,
	client kubernetesClient,
	namespace string,
	targetID uuid.UUID,
	description string,
	ready func([]kubernetesPod) bool,
) []kubernetesPod {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last []kubernetesPod
	for {
		pods, err := client.ListPods(ctx, namespace, targetID)
		if err == nil {
			last = pods
			if ready(pods) {
				return pods
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: last Pods=%#v: %v", description, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForOrbStackKubernetesPodUIDAbsent(
	ctx context.Context,
	client kubernetesClient,
	namespace string,
	targetID uuid.UUID,
	podUID string,
) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pods, err := client.ListPods(ctx, namespace, targetID)
		if err == nil {
			found := false
			for _, pod := range pods {
				if pod.UID == podUID {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for deleted Pod UID %s to disappear: %w", podUID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func orbStackKubernetesWarmPodsBySlot(t *testing.T, pods []kubernetesPod) map[string]kubernetesPod {
	t.Helper()
	result := make(map[string]kubernetesPod, len(pods))
	for _, pod := range pods {
		slot := pod.Labels[kubernetesWarmSlotLabel]
		if slot == "" || pod.UID == "" {
			t.Fatalf("invalid warm Pod identity: %#v", pod)
		}
		result[slot] = pod
	}
	if len(result) != 2 {
		t.Fatalf("warm Pods by slot = %#v, want two slots", result)
	}
	return result
}

func loadOrbStackKubernetesWarmAuthority(
	t *testing.T,
	db *gorm.DB,
	pool persistence.WorkerPool,
) persistence.WorkerPoolWarmCapacity {
	t.Helper()
	var authority persistence.WorkerPoolWarmCapacity
	if err := db.Where("worker_pool_id = ? AND worker_pool_version = ?", pool.ID, pool.Version).
		Take(&authority).Error; err != nil {
		t.Fatal(err)
	}
	return authority
}

func assertOrbStackKubernetesWarmAuthority(
	t *testing.T,
	authority persistence.WorkerPoolWarmCapacity,
	minIdle, desiredTotal, readyIdle int,
) {
	t.Helper()
	if !authority.WarmSupported || authority.MinIdleUnits != minIdle ||
		authority.DesiredIdleUnits != 2 || authority.MaxActiveUnits != 2 ||
		authority.DesiredTotalUnits != desiredTotal || authority.ClaimedUnits != 0 ||
		authority.ReadyIdleUnits != readyIdle ||
		!strings.HasPrefix(authority.Source, managedKubernetesWarmCapacityPublisherPrefix) {
		t.Fatalf("warm capacity authority = %#v", authority)
	}
}
