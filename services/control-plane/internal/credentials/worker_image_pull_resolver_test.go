package credentials

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestResolveWorkerImagePullForTargetFencesBindingScopeAndCredentialVersion(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeTenant, Name: "Worker registry", Purpose: PurposeRegistry,
		Provider: RegistryProviderOci, CredentialType: RegistryBasicCredentialType,
		Payload: map[string]any{"host": "ghcr.io", "username": "synara", "password": "registry-secret-v1"},
	}, "worker-image-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	binding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ExecutionTargetID: &fixture.targetID,
		CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
		SelectorValue: "ghcr.io", CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
	}
	if err := fixture.db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}

	resolution, err := fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	resolved := resolution.Credential
	if resolved == nil || resolved.BindingID != binding.ID || resolved.CredentialID != credential.ID ||
		resolved.CredentialVersion != 1 || resolved.Host != "ghcr.io" || resolved.Username != "synara" ||
		resolved.Password != "registry-secret-v1" || resolved.RegistryToken != "" || !resolution.Authoritative {
		t.Fatalf("resolved Worker image pull Credential = %#v", resolved)
	}

	rotated, err := fixture.service.Rotate(ctx, fixture.owner, fixture.tenantID, credential.ID, RotateInput{
		ExpectedVersion: 1,
		Payload:         map[string]any{"host": "ghcr.io", "username": "synara", "password": "registry-secret-v2"},
	}, "worker-image-rotate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	resolved = resolution.Credential
	if resolved == nil || resolved.CredentialVersion != rotated.Version || resolved.Password != "registry-secret-v2" {
		t.Fatalf("rotated Worker image pull Credential = %#v", resolved)
	}

	if err := fixture.db.Model(&persistence.CredentialBinding{}).Where("id = ?", binding.ID).
		Updates(map[string]any{"disabled_at": fixture.now, "disabled_by": fixture.owner.UserID}).Error; err != nil {
		t.Fatal(err)
	}
	resolution, err = fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "ghcr.io")
	if err != nil || resolution.Credential != nil || !resolution.Authoritative {
		t.Fatalf("disabled Binding resolution = %#v, err = %v", resolution, err)
	}
}

func TestResolveWorkerImagePullForTargetRejectsRevokedCredential(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeTenant, Name: "Worker registry token", Purpose: PurposeRegistry,
		Provider: RegistryProviderOci, CredentialType: RegistryBearerCredentialType,
		Payload: map[string]any{"host": "registry.example.com", "token": "registry-bearer-secret"},
	}, "worker-image-token-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	binding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ExecutionTargetID: &fixture.targetID,
		CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
		SelectorValue: "registry.example.com", CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
	}
	if err := fixture.db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Revoke(ctx, fixture.owner, fixture.tenantID, credential.ID, "worker-image-token-revoke", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	resolution, err := fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "registry.example.com")
	assertCredentialProblemCode(t, err, "worker_image_pull_credential_unavailable")
	if !resolution.Authoritative || resolution.Credential != nil {
		t.Fatalf("revoked Worker image pull resolution = %#v", resolution)
	}
}

func TestResolveWorkerImagePullForTargetRejectsMultipleActiveBindings(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	if err := fixture.db.Exec("DROP INDEX uq_credential_bindings_active_worker_image_target").Error; err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		host     string
		password string
	}{
		{host: "ghcr.io", password: "ghcr-registry-secret"},
		{host: "registry.example.com", password: "example-registry-secret"},
	} {
		credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
			Scope: credentialscope.ScopeTenant, Name: item.host, Purpose: PurposeRegistry,
			Provider: RegistryProviderOci, CredentialType: RegistryBasicCredentialType,
			Payload: map[string]any{"host": item.host, "username": "synara", "password": item.password},
		}, "worker-image-selector-create-"+item.host, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.Create(&persistence.CredentialBinding{
			ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
			ExecutionTargetID: &fixture.targetID,
			CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
			SelectorValue: item.host, CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	resolution, err := fixture.service.ResolveWorkerImagePullForTarget(
		ctx, fixture.tenantID, fixture.targetID, "ghcr.io",
	)
	assertCredentialProblemCode(t, err, "worker_image_pull_binding_ambiguous")
	if !resolution.Authoritative || resolution.Credential != nil {
		t.Fatalf("multiple active Binding resolution = %#v", resolution)
	}
}

func TestResolveWorkerImagePullForTargetNormalizesDockerHubAlias(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeTenant, Name: "Docker Hub", Purpose: PurposeRegistry,
		Provider: RegistryProviderOci, CredentialType: RegistryBasicCredentialType,
		Payload: map[string]any{
			"host": "index.docker.io", "username": "synara", "password": "docker-hub-secret",
		},
	}, "worker-image-docker-hub-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ExecutionTargetID: &fixture.targetID,
		CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
		SelectorValue: "index.docker.io", CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	resolution, err := fixture.service.ResolveWorkerImagePullForTarget(
		ctx, fixture.tenantID, fixture.targetID, "docker.io",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Authoritative || resolution.Credential == nil || resolution.Credential.Host != "index.docker.io" {
		t.Fatalf("Docker Hub alias resolution = %#v", resolution)
	}
}

func TestResolveWorkerImagePullForTargetRevalidatesRevocationAfterDecrypt(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeTenant, Name: "Worker registry", Purpose: PurposeRegistry,
		Provider: RegistryProviderOci, CredentialType: RegistryBasicCredentialType,
		Payload: map[string]any{"host": "ghcr.io", "username": "synara", "password": "registry-secret-v1"},
	}, "worker-image-cas-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	binding := persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ExecutionTargetID: &fixture.targetID,
		CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
		SelectorValue: "ghcr.io", CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
	}
	if err := fixture.db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}

	delegate, err := credentialkms.NewLocalKeyWrapper("credential-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingWorkerImagePullKeyWrapper{
		delegate: delegate, started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	fixture.service.cipher = credentialkms.NewEnvelopeCipher(blocking)
	type result struct {
		resolution WorkerImagePullResolution
		err        error
	}
	completed := make(chan result, 1)
	go func() {
		resolution, resolveErr := fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "ghcr.io")
		completed <- result{resolution: resolution, err: resolveErr}
	}()
	<-blocking.started
	if err := fixture.service.Revoke(
		ctx, fixture.owner, fixture.tenantID, credential.ID, "worker-image-cas-revoke", "127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	resolved := <-completed
	assertCredentialProblemCode(t, resolved.err, "worker_image_pull_credential_unavailable")
	if !resolved.resolution.Authoritative || resolved.resolution.Credential != nil {
		t.Fatalf("post-decrypt revoked resolution = %#v", resolved.resolution)
	}
}

func TestResolveWorkerImagePullForTargetDoesNotAuthorizeTransientKMSFailure(t *testing.T) {
	fixture := newCredentialFixture(t)
	ctx := context.Background()
	credential, err := fixture.service.Create(ctx, fixture.owner, fixture.tenantID, CreateInput{
		Scope: credentialscope.ScopeTenant, Name: "Worker registry", Purpose: PurposeRegistry,
		Provider: RegistryProviderOci, CredentialType: RegistryBasicCredentialType,
		Payload: map[string]any{"host": "ghcr.io", "username": "synara", "password": "registry-secret-v1"},
	}, "worker-image-kms-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.CredentialBinding{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		ExecutionTargetID: &fixture.targetID,
		CredentialID:      credential.ID, BindingKind: workerImagePullBindingKind,
		SelectorValue: "ghcr.io", CreatedBy: fixture.owner.UserID, CreatedAt: fixture.now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.cipher = credentialkms.NewEnvelopeCipher(failingWorkerImagePullKeyWrapper{})
	resolution, err := fixture.service.ResolveWorkerImagePullForTarget(ctx, fixture.tenantID, fixture.targetID, "ghcr.io")
	assertCredentialProblemCode(t, err, "credential_decryption_failed")
	if resolution.Authoritative || resolution.Credential != nil {
		t.Fatalf("transient KMS failure became authoritative: %#v", resolution)
	}
}

type blockingWorkerImagePullKeyWrapper struct {
	delegate credentialkms.KeyWrapper
	started  chan struct{}
	release  chan struct{}
}

func (w *blockingWorkerImagePullKeyWrapper) Provider() string { return w.delegate.Provider() }
func (w *blockingWorkerImagePullKeyWrapper) KeyID() string    { return w.delegate.KeyID() }
func (w *blockingWorkerImagePullKeyWrapper) WrapKey(ctx context.Context, key, aad []byte) ([]byte, error) {
	return w.delegate.WrapKey(ctx, key, aad)
}
func (w *blockingWorkerImagePullKeyWrapper) UnwrapKey(ctx context.Context, encrypted, aad []byte) ([]byte, error) {
	select {
	case w.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.release:
		return w.delegate.UnwrapKey(ctx, encrypted, aad)
	}
}

type failingWorkerImagePullKeyWrapper struct{}

func (failingWorkerImagePullKeyWrapper) Provider() string { return "local" }
func (failingWorkerImagePullKeyWrapper) KeyID() string    { return "credential-test-v1" }
func (failingWorkerImagePullKeyWrapper) WrapKey(context.Context, []byte, []byte) ([]byte, error) {
	return nil, errors.New("unexpected wrap")
}
func (failingWorkerImagePullKeyWrapper) UnwrapKey(context.Context, []byte, []byte) ([]byte, error) {
	return nil, errors.New("KMS temporarily unavailable")
}
