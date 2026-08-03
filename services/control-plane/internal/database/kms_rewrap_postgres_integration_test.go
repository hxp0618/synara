package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresKMSRewrapMigrationGuardsEnvelopeOnlyChanges(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-kms-rewrap-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	credential := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: domain.TenantID, Scope: "tenant", Name: "KMS guard",
		Purpose: "provider", Provider: "openai", CredentialType: "api_key",
		EncryptedPayload: []byte("credential-payload"), EncryptedDataKey: []byte("credential-old-key"),
		KMSProvider: "local", KMSKeyID: "old-v1", AADVersion: 3, Version: 1,
		CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCredential{}).Where("id = ?", credential.ID).
		UpdateColumn("encrypted_data_key", []byte("unauthorized")).Error; err == nil {
		t.Fatal("PostgreSQL accepted an unauthorized Credential envelope-only change")
	}
	run := persistence.KMSRewrapRun{
		ID: uuid.New(), PrimaryProvider: "local", PrimaryKeyID: "new-v2",
		OperatorReference: "change-postgres-kms", StartedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	credentialNewKey := []byte("credential-new-key")
	credentialEntry := persistence.KMSRewrapEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: domain.TenantID,
		ResourceType: "provider_credential", ResourceID: credential.ID,
		OldKMSProvider: "local", OldKMSKeyID: "old-v1", OldEncryptedDataKey: credential.EncryptedDataKey,
		NewKMSProvider: "local", NewKMSKeyID: "new-v2", NewEncryptedDataKey: credentialNewKey,
		CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&credentialEntry).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.ProviderCredential{}).Where("id = ?", credential.ID).
			UpdateColumns(map[string]any{
				"encrypted_data_key": credentialNewKey, "kms_provider": "local", "kms_key_id": "new-v2",
			}).Error
	}); err != nil {
		t.Fatal(err)
	}

	provider := "local"
	oldKeyID := "old-v1"
	clientID := "synara-client"
	connection := persistence.IdentityConnection{
		ID: uuid.New(), TenantID: domain.TenantID, Kind: "oidc", Name: "KMS Identity",
		Status: "active", Issuer: "https://identity.example.com", ClientID: &clientID,
		EncryptedSecret: []byte("identity-payload"), EncryptedDataKey: []byte("identity-old-key"),
		KMSProvider: &provider, KMSKeyID: &oldKeyID, Configuration: map[string]any{},
		CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.IdentityConnection{}).Where("id = ?", connection.ID).
		UpdateColumn("encrypted_data_key", []byte("unauthorized")).Error; err == nil {
		t.Fatal("PostgreSQL accepted an unauthorized identity envelope-only change")
	}
	if err := db.Model(&persistence.IdentityConnection{}).Where("id = ?", connection.ID).
		UpdateColumns(map[string]any{
			"encrypted_secret":   []byte("rotated-identity-payload"),
			"encrypted_data_key": []byte("rotated-identity-key"),
		}).Error; err != nil {
		t.Fatalf("PostgreSQL rejected a normal identity secret rotation: %v", err)
	}

	attempt := persistence.IdentityLoginAttempt{
		ID: uuid.New(), TenantID: domain.TenantID, ConnectionID: connection.ID,
		StateHash: []byte("state-hash"), EncryptedPayload: []byte("attempt-payload"),
		EncryptedDataKey: []byte("attempt-old-key"), KMSProvider: "local", KMSKeyID: "old-v1",
		ReturnTo: "/", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.IdentityLoginAttempt{}).Where("id = ?", attempt.ID).
		UpdateColumn("encrypted_data_key", []byte("unauthorized")).Error; err == nil {
		t.Fatal("PostgreSQL accepted an unauthorized Login Attempt envelope-only change")
	}
	connectionNewKey := []byte("identity-new-key")
	connectionEntry := persistence.KMSRewrapEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: domain.TenantID,
		ResourceType: "identity_connection", ResourceID: connection.ID,
		OldKMSProvider: "local", OldKMSKeyID: "old-v1", OldEncryptedDataKey: []byte("rotated-identity-key"),
		NewKMSProvider: "local", NewKMSKeyID: "new-v2", NewEncryptedDataKey: connectionNewKey,
		CreatedAt: now,
	}
	attemptNewKey := []byte("attempt-new-key")
	attemptEntry := persistence.KMSRewrapEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: domain.TenantID,
		ResourceType: "identity_login_attempt", ResourceID: attempt.ID,
		OldKMSProvider: "local", OldKMSKeyID: "old-v1", OldEncryptedDataKey: attempt.EncryptedDataKey,
		NewKMSProvider: "local", NewKMSKeyID: "new-v2", NewEncryptedDataKey: attemptNewKey,
		CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&connectionEntry).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.IdentityConnection{}).Where("id = ?", connection.ID).
			UpdateColumns(map[string]any{
				"encrypted_data_key": connectionNewKey, "kms_provider": "local", "kms_key_id": "new-v2",
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&attemptEntry).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.IdentityLoginAttempt{}).Where("id = ?", attempt.ID).
			UpdateColumns(map[string]any{
				"encrypted_data_key": attemptNewKey, "kms_provider": "local", "kms_key_id": "new-v2",
			}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.KMSRewrapReceipt{
		RunID: run.ID, ProviderCredentialCount: 1, IdentityConnectionCount: 1,
		IdentityLoginAttemptCount: 1, EntryDigest: make([]byte, 32), CompletedAt: now,
	}).Error; err != nil {
		t.Fatalf("PostgreSQL rejected a converged KMS rewrap receipt: %v", err)
	}
	if err := db.Create(&persistence.KMSRewrapEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: domain.TenantID,
		ResourceType: "provider_credential", ResourceID: uuid.New(),
		OldKMSProvider: "local", OldKMSKeyID: "old-v1", OldEncryptedDataKey: []byte("late-old"),
		NewKMSProvider: "local", NewKMSKeyID: "new-v2", NewEncryptedDataKey: []byte("late-new"), CreatedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted new evidence after a KMS rewrap receipt")
	}

	if err := db.Model(&persistence.KMSRewrapEntry{}).Where("id = ?", credentialEntry.ID).
		UpdateColumn("new_kms_key_id", "tampered").Error; err == nil {
		t.Fatal("PostgreSQL allowed KMS rewrap evidence mutation")
	}
}
