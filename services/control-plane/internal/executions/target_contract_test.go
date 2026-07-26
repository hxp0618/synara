package executions

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRemoteWorkerRequiresLeaseAndFencingAndCannotSwitchTargets(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-target-test")
	if err != nil {
		t.Fatal(err)
	}
	sshTarget := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "docker", Name: "docker-test", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: map[string]any{},
	}
	if err := store.DB().Create(&sshTarget).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), config, nil)
	service := NewService(store.DB(), nil, 30*time.Second, 90*time.Second, time.Hour, nil, targetService)
	input := RegisterWorkerInput{
		ExecutionTargetID: sshTarget.ID, TargetKind: "docker", ClusterID: "local",
		InstanceUID: uuid.NewString(),
		Namespace:   "default", PodName: "agentd-test", Version: "test", ProtocolVersion: WorkerProtocolVersion,
	}
	assertUnsupportedProtocol := func(label string, err error, received int) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Status != 426 || apiError.Code != "worker_protocol_version_unsupported" ||
			apiError.Details["received"] != received || apiError.Details["minimumSupported"] != WorkerProtocolVersion ||
			apiError.Details["maximumSupported"] != WorkerProtocolVersion {
			t.Fatalf("%s returned unexpected Worker Protocol rejection: %#v", label, apiError)
		}
	}
	for _, legacyProtocolVersion := range []int{0, 1} {
		legacyInput := input
		legacyInput.ProtocolVersion = legacyProtocolVersion
		legacyInput.InstanceUID = ""
		_, err := service.Register(ctx, legacyInput)
		assertUnsupportedProtocol("legacy registration", err, legacyProtocolVersion)
	}
	if _, err := service.Register(ctx, input); err == nil {
		t.Fatal("remote worker without lease/fencing support was accepted")
	}
	input.LeaseSupported = true
	input.FencingSupported = true
	input.Capabilities = map[string]any{"workspaceModes": []string{"local"}}
	input.ProtocolVersion = WorkerProtocolVersion + 1
	_, err = service.Register(ctx, input)
	assertUnsupportedProtocol("future registration", err, WorkerProtocolVersion+1)
	input.ProtocolVersion = WorkerProtocolVersion
	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	firstWorkerID := worker.ID
	if worker.Incarnation != 1 || worker.InstanceUID != input.InstanceUID {
		t.Fatalf("initial Worker identity fence is invalid: %#v", worker)
	}
	_, err = service.Heartbeat(ctx, worker, HeartbeatInput{})
	assertUnsupportedProtocol("missing heartbeat protocol", err, 0)
	input.Version = "test-v2"
	input.InstanceUID = uuid.NewString()
	input.Capabilities = map[string]any{"workspaceModes": []string{"local", "worktree"}}
	reregistered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatalf("re-register SQLite worker with JSON capabilities: %v", err)
	}
	if _, err := service.Authenticate(ctx, registered.Token); err == nil {
		t.Fatal("re-registration did not revoke the previous Worker token")
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != firstWorkerID || worker.Incarnation != 2 || worker.InstanceUID != input.InstanceUID {
		t.Fatalf("Worker re-registration did not atomically rotate its identity fence: %#v", worker)
	}
	draining := true
	heartbeat, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		Version: "test-v3", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: map[string]any{"workspaceModes": []string{"worktree"}}, Draining: &draining,
	})
	if err != nil {
		t.Fatalf("heartbeat SQLite worker with JSON capabilities: %v", err)
	}
	workspaceModes, ok := heartbeat.Capabilities["workspaceModes"].([]any)
	if !ok || len(workspaceModes) != 1 || workspaceModes[0] != "worktree" {
		t.Fatalf("worker heartbeat did not persist JSON capabilities: %#v", heartbeat.Capabilities)
	}
	if heartbeat.Status != "draining" || heartbeat.DrainingAt == nil || heartbeat.ProtocolVersion != WorkerProtocolVersion {
		t.Fatalf("worker heartbeat did not persist drain/protocol state: %#v", heartbeat)
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: sshTarget.ID, TargetKind: "docker",
	}, "claim-draining"); err == nil {
		t.Fatal("draining Worker was allowed to claim")
	}
	staleHeartbeat := time.Now().UTC().Add(-2 * time.Minute)
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("last_heartbeat_at", staleHeartbeat).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.markStaleWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	var staleWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", worker.ID).Take(&staleWorker).Error; err != nil {
		t.Fatal(err)
	}
	if staleWorker.Status != "offline" {
		t.Fatalf("stale Draining Worker was not marked offline: %#v", staleWorker)
	}
	draining = false
	if _, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, Draining: &draining,
	}); err != nil {
		t.Fatalf("worker could not leave drain mode: %v", err)
	}
	worker, err = service.Authenticate(ctx, reregistered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
	}, "claim-wrong-target"); err == nil {
		t.Fatal("worker claimed against an incompatible execution target")
	}
}

func TestRegisterRejectsUntrustedProcessContainmentProofByDefault(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-containment-default-trust-test")
	if err != nil {
		t.Fatal(err)
	}
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "ssh", Name: "ssh-default-containment", Status: "active", ConfigurationEncrypted: []byte{},
		Capabilities: map[string]any{
			"providerPolicy": map[string]any{
				"experimentalProviders": []any{"codex", "claudeAgent"},
			},
		},
	}
	if err := store.DB().Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), config, nil)
	service := NewService(store.DB(), nil, 30*time.Second, 90*time.Second, time.Hour, nil, targetService)
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	instanceUID := uuid.New()
	const bootstrapGeneration int64 = 1
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_generation": bootstrapGeneration,
			"ssh_operation_kind": "install", "ssh_operation_started_at": time.Now().UTC(),
			"ssh_expected_instance_uid": instanceUID,
		}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Register(ctx, RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: "ssh",
		InstanceUID: instanceUID.String(), SSHBootstrapGeneration: func() *int64 { value := bootstrapGeneration; return &value }(),
		ClusterID: "default-trust", Namespace: "default", PodName: "worker-default-trust",
		Version: "worker-test", ProtocolVersion: WorkerProtocolVersion,
		Capabilities: capabilities, LeaseSupported: true, FencingSupported: true,
	})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != 409 || apiError.Code != "worker_containment_untrusted" {
		t.Fatalf("containment trust problem = %#v", err)
	}
}

func TestSSHBootstrapAuthorityFencesRegistrationAndHeartbeat(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "ssh-bootstrap-authority-test")
	if err != nil {
		t.Fatal(err)
	}
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &domain.TenantID, OrganizationID: &domain.OrganizationID,
		Kind: "ssh", Name: "ssh-bootstrap-authority", Status: "offline", ConfigurationEncrypted: []byte{},
		Capabilities: workerManifestTestTargetCapabilities(),
	}
	if err := store.DB().Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), config, nil)
	service := NewService(store.DB(), nil, 30*time.Second, 90*time.Second, time.Hour, nil, targetService)
	expectedInstanceUID := uuid.New()
	wrongInstanceUID := uuid.New()
	const generation int64 = 9
	input := RegisterWorkerInput{
		ExecutionTargetID: target.ID, TargetKind: "ssh", InstanceUID: expectedInstanceUID.String(),
		SSHBootstrapGeneration: func() *int64 { value := generation; return &value }(),
		ClusterID:              "ssh", Namespace: "default", PodName: "ssh-" + target.ID.String(), Version: "worker-test",
		ProtocolVersion: WorkerProtocolVersion, Capabilities: workerManifestTestCapabilities(),
		LeaseSupported: true, FencingSupported: true,
	}
	assertBootstrapRejected := func(label string, err error) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Code != "ssh_bootstrap_authority_invalid" {
			t.Fatalf("%s problem = %#v (%v)", label, apiError, err)
		}
	}
	assertActiveRestartRejected := func(label string, err error) {
		t.Helper()
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Code != "ssh_active_reregistration_invalid" {
			t.Fatalf("%s problem = %#v (%v)", label, apiError, err)
		}
	}

	_, err = service.Register(ctx, input)
	assertBootstrapRejected("offline without operation", err)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Updates(map[string]any{
			"ssh_operation_generation": generation, "ssh_operation_kind": "revoke",
			"ssh_operation_started_at": time.Now().UTC(), "ssh_expected_instance_uid": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Register(ctx, input)
	assertBootstrapRejected("revoke window", err)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Updates(map[string]any{
			"ssh_operation_kind": "install", "ssh_expected_instance_uid": expectedInstanceUID,
		}).Error; err != nil {
		t.Fatal(err)
	}
	wrongGenerationInput := input
	wrongGenerationInput.SSHBootstrapGeneration = func() *int64 { value := generation - 1; return &value }()
	_, err = service.Register(ctx, wrongGenerationInput)
	assertBootstrapRejected("old generation", err)
	wrongInstanceInput := input
	wrongInstanceInput.InstanceUID = wrongInstanceUID.String()
	_, err = service.Register(ctx, wrongInstanceInput)
	assertBootstrapRejected("wrong instance", err)

	registered, err := service.Register(ctx, input)
	if err != nil {
		t.Fatalf("correct install bootstrap registration: %v", err)
	}
	worker, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Heartbeat(ctx, worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, SSHBootstrapGeneration: input.SSHBootstrapGeneration,
	}); err != nil {
		t.Fatalf("correct install bootstrap heartbeat: %v", err)
	}
	wrongHeartbeatGeneration := generation - 1
	_, err = service.Heartbeat(ctx, worker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion, SSHBootstrapGeneration: &wrongHeartbeatGeneration,
	})
	assertBootstrapRejected("old-generation heartbeat", err)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Updates(map[string]any{
			"status": "active", "ssh_operation_kind": nil,
			"ssh_operation_started_at": nil, "ssh_expected_instance_uid": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	restarted, err := service.Register(ctx, input)
	if err != nil {
		t.Fatalf("active SSH Worker restart registration: %v", err)
	}
	restartedWorker, err := service.Authenticate(ctx, restarted.Token)
	if err != nil {
		t.Fatal(err)
	}
	if restartedWorker.ID != worker.ID || restartedWorker.Incarnation != worker.Incarnation+1 {
		t.Fatalf("active SSH restart did not rotate the current incarnation: %#v", restartedWorker)
	}
	if _, err := service.Authenticate(ctx, registered.Token); err == nil {
		t.Fatal("active SSH restart did not revoke the previous bearer token")
	}
	_, err = service.Register(ctx, wrongInstanceInput)
	assertActiveRestartRejected("active wrong instance", err)
	_, err = service.Register(ctx, wrongGenerationInput)
	assertActiveRestartRejected("active wrong generation", err)
	newLogicalIdentity := input
	newLogicalIdentity.PodName = "new-logical-identity"
	_, err = service.Register(ctx, newLogicalIdentity)
	assertActiveRestartRejected("active new logical identity", err)

	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", restartedWorker.ID).
		Update("ssh_bootstrap_generation", nil).Error; err != nil {
		t.Fatal(err)
	}
	legacyInput := input
	legacyInput.SSHBootstrapGeneration = nil
	legacyRestarted, err := service.Register(ctx, legacyInput)
	if err != nil {
		t.Fatalf("legacy active SSH restart registration: %v", err)
	}
	legacyWorker, err := service.Authenticate(ctx, legacyRestarted.Token)
	if err != nil {
		t.Fatal(err)
	}
	if legacyWorker.SSHBootstrapGeneration != nil {
		t.Fatalf("legacy SSH restart unexpectedly invented bootstrap authority: %#v", legacyWorker.SSHBootstrapGeneration)
	}
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Updates(map[string]any{
			"status": "offline", "ssh_operation_kind": nil,
			"ssh_operation_started_at": nil, "ssh_expected_instance_uid": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Heartbeat(ctx, legacyWorker, HeartbeatInput{
		ProtocolVersion: WorkerProtocolVersion,
	})
	assertBootstrapRejected("offline without operation heartbeat", err)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	if _, err := service.RevokeWorker(
		ctx, principal, domain.TenantID, legacyWorker.ID,
		RevokeWorkerInput{ExpectedIncarnation: legacyWorker.Incarnation, Reason: "active SSH restart revoked"},
		"active-ssh-restart-revoke", "active-ssh-restart-revoke", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	_, err = service.Register(ctx, legacyInput)
	var revokedProblem *problem.Error
	if !errors.As(err, &revokedProblem) || revokedProblem.Code != "worker_identity_revoked" {
		t.Fatalf("revoked active SSH Worker restart problem = %#v (%v)", revokedProblem, err)
	}
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", target.ID).
		Update("status", "disabled").Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Register(ctx, input)
	assertBootstrapRejected("disabled registration", err)
}
