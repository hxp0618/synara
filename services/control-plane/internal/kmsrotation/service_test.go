package kmsrotation

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestOnlineRewrapIsPreflightedAuditedImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "kms-rewrap-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	oldWrapper, err := credentialkms.NewLocalKeyWrapper("local-old-v1", bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	newWrapper, err := credentialkms.NewLocalKeyWrapper("local-new-v2", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	oldCipher := credentialkms.NewEnvelopeCipher(oldWrapper)
	keyring, err := credentialkms.NewEnvelopeCipherWithDecryptors(newWrapper, oldWrapper)
	if err != nil {
		t.Fatal(err)
	}

	credential := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: domain.TenantID, Scope: "tenant", Name: "Production OpenAI",
		Purpose: credentials.PurposeProvider, Provider: "openai", CredentialType: "api_key",
		AADVersion: 3, Version: 1, CreatedBy: domain.UserID, UpdatedBy: domain.UserID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	credentialAAD, err := credentials.EnvelopeAAD(credential)
	if err != nil {
		t.Fatal(err)
	}
	credentialEnvelope, err := oldCipher.Encrypt(ctx, []byte(`{"apiKey":"secret"}`), credentialAAD)
	if err != nil {
		t.Fatal(err)
	}
	applyEnvelopeToCredential(&credential, credentialEnvelope)
	if err := store.DB().Create(&credential).Error; err != nil {
		t.Fatal(err)
	}

	connection := persistence.IdentityConnection{
		ID: uuid.New(), TenantID: domain.TenantID, Kind: "oidc", Name: "Company SSO",
		Status: "active", Issuer: "https://identity.example.com", Configuration: map[string]any{},
		CreatedBy: domain.UserID, UpdatedBy: domain.UserID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	connectionEnvelope, err := oldCipher.Encrypt(
		ctx, []byte(`{"clientSecret":"identity-secret"}`),
		enterpriseidentity.ConnectionEnvelopeAAD(connection.TenantID, connection.ID, connection.Kind),
	)
	if err != nil {
		t.Fatal(err)
	}
	connection.EncryptedSecret = connectionEnvelope.EncryptedPayload
	connection.EncryptedDataKey = connectionEnvelope.EncryptedDataKey
	connection.KMSProvider = &connectionEnvelope.KMSProvider
	connection.KMSKeyID = &connectionEnvelope.KMSKeyID
	if err := store.DB().Create(&connection).Error; err != nil {
		t.Fatal(err)
	}

	attempt := persistence.IdentityLoginAttempt{
		ID: uuid.New(), TenantID: domain.TenantID, ConnectionID: connection.ID,
		StateHash: bytes.Repeat([]byte{0x11}, 32), ReturnTo: "/settings",
		ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
	}
	attemptEnvelope, err := oldCipher.Encrypt(
		ctx, []byte(`{"nonce":"nonce","verifier":"verifier"}`),
		enterpriseidentity.LoginAttemptEnvelopeAAD(attempt.TenantID, attempt.ID, attempt.ConnectionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt.EncryptedPayload = attemptEnvelope.EncryptedPayload
	attempt.EncryptedDataKey = attemptEnvelope.EncryptedDataKey
	attempt.KMSProvider = attemptEnvelope.KMSProvider
	attempt.KMSKeyID = attemptEnvelope.KMSKeyID
	if err := store.DB().Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}

	newOnlyService, err := New(store.DB(), credentialkms.NewEnvelopeCipher(newWrapper))
	if err != nil {
		t.Fatal(err)
	}
	unconfiguredPlan, err := newOnlyService.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unconfiguredPlan.PendingRewrap != 3 || unconfiguredPlan.Unconfigured != 3 {
		t.Fatalf("unexpected missing-key preflight: %#v", unconfiguredPlan)
	}
	if _, err := newOnlyService.Execute(ctx, ExecuteOptions{OperatorReference: "change-123"}); err == nil || !strings.Contains(err.Error(), "configure every old key") {
		t.Fatalf("expected fail-closed missing-key execution, got %v", err)
	}
	assertCount(t, store.DB(), &persistence.KMSRewrapRun{}, 0)

	service, err := New(store.DB(), keyring)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PendingRewrap != 3 || plan.Unconfigured != 0 || plan.AlreadyPrimary != 0 {
		t.Fatalf("unexpected rewrap plan: %#v", plan)
	}
	assertCount(t, store.DB(), &persistence.KMSRewrapRun{}, 0)

	if err := store.DB().Model(&persistence.ProviderCredential{}).Where("id = ?", credential.ID).
		UpdateColumn("encrypted_data_key", []byte("unauthorized")).Error; err == nil {
		t.Fatal("Provider Credential envelope changed without rewrap authorization")
	}
	if err := store.DB().Model(&persistence.IdentityConnection{}).Where("id = ?", connection.ID).
		UpdateColumn("encrypted_data_key", []byte("unauthorized")).Error; err == nil {
		t.Fatal("identity Connection envelope changed without rewrap authorization")
	}

	report, err := service.Execute(ctx, ExecuteOptions{OperatorReference: "change-123", BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProviderCredentialCount != 1 || report.IdentityConnectionCount != 1 ||
		report.IdentityLoginAttemptCount != 1 || len(report.EntryDigest) != 64 {
		t.Fatalf("unexpected KMS rewrap receipt: %#v", report)
	}

	var rewrappedCredential persistence.ProviderCredential
	if err := store.DB().First(&rewrappedCredential, "id = ?", credential.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rewrappedCredential.Version != credential.Version ||
		!bytes.Equal(rewrappedCredential.EncryptedPayload, credential.EncryptedPayload) ||
		bytes.Equal(rewrappedCredential.EncryptedDataKey, credential.EncryptedDataKey) ||
		rewrappedCredential.KMSKeyID != "local-new-v2" || !rewrappedCredential.UpdatedAt.Equal(credential.UpdatedAt) {
		t.Fatalf("Provider Credential business state changed during rewrap: %#v", rewrappedCredential)
	}
	if _, err := keyring.Decrypt(ctx, credentialkms.Envelope{
		EncryptedPayload: rewrappedCredential.EncryptedPayload,
		EncryptedDataKey: rewrappedCredential.EncryptedDataKey,
		KMSProvider:      rewrappedCredential.KMSProvider, KMSKeyID: rewrappedCredential.KMSKeyID,
	}, credentialAAD); err != nil {
		t.Fatalf("new primary could not decrypt rewrapped Credential: %v", err)
	}

	var rewrappedConnection persistence.IdentityConnection
	if err := store.DB().First(&rewrappedConnection, "id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewrappedConnection.EncryptedSecret, connection.EncryptedSecret) ||
		bytes.Equal(rewrappedConnection.EncryptedDataKey, connection.EncryptedDataKey) ||
		rewrappedConnection.KMSKeyID == nil || *rewrappedConnection.KMSKeyID != "local-new-v2" ||
		!rewrappedConnection.UpdatedAt.Equal(connection.UpdatedAt) {
		t.Fatalf("identity Connection business state changed during rewrap: %#v", rewrappedConnection)
	}
	var rewrappedAttempt persistence.IdentityLoginAttempt
	if err := store.DB().First(&rewrappedAttempt, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewrappedAttempt.EncryptedPayload, attempt.EncryptedPayload) ||
		bytes.Equal(rewrappedAttempt.EncryptedDataKey, attempt.EncryptedDataKey) ||
		rewrappedAttempt.KMSKeyID != "local-new-v2" {
		t.Fatalf("identity Login Attempt business state changed during rewrap: %#v", rewrappedAttempt)
	}

	completedPlan, err := service.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completedPlan.PendingRewrap != 0 || completedPlan.AlreadyPrimary != 3 {
		t.Fatalf("unexpected completed plan: %#v", completedPlan)
	}
	assertCount(t, store.DB(), &persistence.KMSRewrapEntry{}, 3)
	assertCount(t, store.DB(), &persistence.KMSRewrapReceipt{}, 1)
	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("action = ?", "security.credential_kms_rewrapped").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("expected 3 KMS rewrap audits, got %d", auditCount)
	}
	if err := store.DB().Model(&persistence.KMSRewrapReceipt{}).Where("run_id = ?", report.RunID).
		UpdateColumn("provider_credential_count", 99).Error; err == nil {
		t.Fatal("KMS rewrap receipt was mutable")
	}
	if err := store.DB().Create(&persistence.KMSRewrapEntry{
		ID: uuid.New(), RunID: report.RunID, TenantID: domain.TenantID,
		ResourceType: ResourceProviderCredential, ResourceID: uuid.New(),
		OldKMSProvider: "local", OldKMSKeyID: "old-v1", OldEncryptedDataKey: []byte("old"),
		NewKMSProvider: "local", NewKMSKeyID: "local-new-v2", NewEncryptedDataKey: []byte("new"),
		CreatedAt: time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("completed KMS rewrap run accepted a new evidence entry")
	}

	second, err := service.Execute(ctx, ExecuteOptions{OperatorReference: "change-124"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ProviderCredentialCount != 0 || second.IdentityConnectionCount != 0 ||
		second.IdentityLoginAttemptCount != 0 {
		t.Fatalf("idempotent rerun unexpectedly rewrapped resources: %#v", second)
	}
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("action = ?", "security.credential_kms_rewrapped").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("idempotent rerun added audits: %d", auditCount)
	}
	malformed := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: domain.TenantID, Scope: "tenant", Name: "Malformed envelope",
		Purpose: credentials.PurposeProvider, Provider: "openai", CredentialType: "api_key",
		EncryptedPayload: []byte("malformed"), EncryptedDataKey: []byte{},
		KMSProvider: "local", KMSKeyID: "local-new-v2", AADVersion: 3, Version: 1,
		CreatedBy: domain.UserID, UpdatedBy: domain.UserID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.DB().Create(&malformed).Error; err != nil {
		t.Fatal(err)
	}
	malformedPlan, err := service.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if malformedPlan.PendingRewrap != 1 || malformedPlan.Unconfigured != 1 {
		t.Fatalf("malformed envelope did not fail preflight: %#v", malformedPlan)
	}
	if _, err := service.Execute(ctx, ExecuteOptions{OperatorReference: "change-125"}); err == nil {
		t.Fatal("malformed primary envelope did not fail closed")
	}
	assertCount(t, store.DB(), &persistence.KMSRewrapRun{}, 2)
}

func applyEnvelopeToCredential(model *persistence.ProviderCredential, envelope credentialkms.Envelope) {
	model.EncryptedPayload = envelope.EncryptedPayload
	model.EncryptedDataKey = envelope.EncryptedDataKey
	model.KMSProvider = envelope.KMSProvider
	model.KMSKeyID = envelope.KMSKeyID
}

func assertCount(t *testing.T, db *gorm.DB, model any, expected int64) {
	t.Helper()
	var actual int64
	if err := db.Model(model).Count(&actual).Error; err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("unexpected %T count: got %d, want %d", model, actual, expected)
	}
}
