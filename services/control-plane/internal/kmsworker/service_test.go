package kmsworker

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type serviceFixture struct {
	t       *testing.T
	service *Service
	db      *gorm.DB
	now     time.Time
}

func newServiceFixture(t *testing.T, sealByte byte) *serviceFixture {
	t.Helper()
	db, err := OpenStore(context.Background(), StoreConfig{
		Driver: "sqlite", SQLitePath: "file:" + uuid.NewString() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateStore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	seals, err := NewSealKeyring(bytes.Repeat([]byte{sealByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &serviceFixture{t: t, db: db, now: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)}
	fixture.service, err = NewService(db, seals, ServiceOptions{
		Now: func() time.Time { return fixture.now }, MinimumDeleteDelay: 7 * 24 * time.Hour,
		InventoryMaximumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return fixture
}

func (f *serviceFixture) createKey(idempotency string) KeyDescription {
	f.t.Helper()
	created, err := f.service.CreateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, CreateKeyInput{
		Name: "Provider credential KEK", Labels: map[string]string{"purpose": "credential-envelope"},
		IdempotencyKey: idempotency,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

func TestCreateWrapUnwrapBindsAADAndDoesNotStorePlaintext(t *testing.T) {
	fixture := newServiceFixture(t, 0x41)
	created := fixture.createKey("create-one")
	if created.ActiveVersion != 1 || len(created.Versions) != 1 || !created.Versions[0].Encryptable {
		t.Fatalf("unexpected create result: %#v", created)
	}

	dataKey := bytes.Repeat([]byte{0x52}, 32)
	aad := []byte("tenant/credential/provider/api-key")
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	wrapped, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, dataKey, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped.WrappedDataKey, dataKey) || wrapped.KeyID != VersionedKeyID(created.KeyID, 1) {
		t.Fatalf("unsafe or wrongly identified wrapped result: %#v", wrapped)
	}
	plaintext, err := fixture.service.Unwrap(context.Background(), cryptor, created.KeyID, 1, wrapped.WrappedDataKey, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, dataKey) {
		t.Fatal("unwrapped data key mismatch")
	}
	if _, err := fixture.service.Unwrap(context.Background(), cryptor, created.KeyID, 1, wrapped.WrappedDataKey, []byte("other-resource")); errorCode(err) != "key_authentication_failed" {
		t.Fatalf("wrong AAD was not rejected: %v", err)
	}

	var stored KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.SealedMaterial, dataKey) || len(stored.SealedMaterial) <= 32 || stored.SealKeyID == "" {
		t.Fatal("managed key was not sealed safely")
	}
	var audits int64
	if err := fixture.db.Model(&AuditEvent{}).Where("key_id = ?", created.KeyID).Count(&audits).Error; err != nil || audits != 3 {
		t.Fatalf("audit count = %d, %v", audits, err)
	}
}

func TestRotateMakesOldVersionDecryptOnlyAndPreservesOldEnvelope(t *testing.T) {
	fixture := newServiceFixture(t, 0x42)
	created := fixture.createKey("create-rotate")
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	dataKey := bytes.Repeat([]byte{0x61}, 32)
	aad := []byte("rotation-aad")
	oldWrapped, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, dataKey, aad)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := fixture.service.RotateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID, RotateKeyInput{
		OperatorReference: "change-123", IdempotencyKey: "rotate-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ActiveVersion != 2 || rotated.Versions[0].State != StateDecryptOnly || rotated.Versions[1].State != StateActive {
		t.Fatalf("unexpected rotation result: %#v", rotated)
	}
	if _, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, dataKey, aad); errorCode(err) != "key_not_encryptable" {
		t.Fatalf("old key still accepted Wrap: %v", err)
	}
	plaintext, err := fixture.service.Unwrap(context.Background(), cryptor, created.KeyID, 1, oldWrapped.WrappedDataKey, aad)
	if err != nil || !bytes.Equal(plaintext, dataKey) {
		t.Fatalf("old envelope no longer decrypts: %x, %v", plaintext, err)
	}
	newWrapped, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 2, dataKey, aad)
	if err != nil || newWrapped.KeyID != VersionedKeyID(created.KeyID, 2) {
		t.Fatalf("new active version not used: %#v, %v", newWrapped, err)
	}
}

func TestExpiryIsEnforcedSynchronously(t *testing.T) {
	fixture := newServiceFixture(t, 0x43)
	encryptExpiry := fixture.now.Add(time.Hour)
	decryptExpiry := fixture.now.Add(2 * time.Hour)
	created, err := fixture.service.CreateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, CreateKeyInput{
		Name: "expiring", EncryptNotAfter: &encryptExpiry, DecryptNotAfter: &decryptExpiry, IdempotencyKey: "create-expiring",
	})
	if err != nil {
		t.Fatal(err)
	}
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	dataKey := bytes.Repeat([]byte{0x71}, 32)
	aad := []byte("expiry-aad")
	wrapped, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, dataKey, aad)
	if err != nil {
		t.Fatal(err)
	}
	fixture.now = encryptExpiry.Add(time.Nanosecond)
	if _, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, dataKey, aad); errorCode(err) != "key_not_encryptable" {
		t.Fatalf("expired key accepted Wrap: %v", err)
	}
	if _, err := fixture.service.Unwrap(context.Background(), cryptor, created.KeyID, 1, wrapped.WrappedDataKey, aad); err != nil {
		t.Fatalf("decrypt grace period was not honored: %v", err)
	}
	fixture.now = decryptExpiry.Add(time.Nanosecond)
	if _, err := fixture.service.Unwrap(context.Background(), cryptor, created.KeyID, 1, wrapped.WrappedDataKey, aad); errorCode(err) != "key_not_decryptable" {
		t.Fatalf("decrypt expiry was not enforced: %v", err)
	}
}

func TestClockRollbackFailsClosed(t *testing.T) {
	fixture := newServiceFixture(t, 0x73)
	created := fixture.createKey("clock-create")
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	if _, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, bytes.Repeat([]byte{0x22}, 32), []byte("clock-aad")); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(-time.Minute)
	if _, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, bytes.Repeat([]byte{0x22}, 32), []byte("clock-aad")); errorCode(err) != "kms_unavailable" {
		t.Fatalf("clock rollback did not fail closed: %v", err)
	}
}

func TestUpdateChangesMetadataAndLifecycleWithoutChangingKeyMaterial(t *testing.T) {
	fixture := newServiceFixture(t, 0x44)
	created := fixture.createKey("create-update")
	var before KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	name := "updated"
	expires := fixture.now.Add(24 * time.Hour)
	updated, err := fixture.service.UpdateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID, UpdateKeyInput{
		Name: &name, EncryptNotAfter: OptionalTime{Present: true, Value: &expires},
		ExpectedRevision: created.MetadataRevision, IdempotencyKey: "update-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != name || updated.MetadataRevision != created.MetadataRevision+1 || updated.Versions[0].EncryptNotAfter == nil {
		t.Fatalf("unexpected update: %#v", updated)
	}
	var after KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.SealedMaterial, after.SealedMaterial) || before.SealKeyID != after.SealKeyID || before.Algorithm != after.Algorithm {
		t.Fatal("metadata update changed cryptographic material")
	}
	if _, err := fixture.service.UpdateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID, UpdateKeyInput{
		Name: &name, ExpectedRevision: created.MetadataRevision, IdempotencyKey: "update-stale",
	}); errorCode(err) != "key_version_conflict" {
		t.Fatalf("stale update was not rejected: %v", err)
	}
}

func TestDelayedDeletionRequiresIndependentFreshZeroInventory(t *testing.T) {
	fixture := newServiceFixture(t, 0x45)
	created := fixture.createKey("create-delete")
	disabler := Actor{Identity: "disabler-a", Role: RoleKeyDisabler}
	disabled, err := fixture.service.DisableKey(context.Background(), disabler, created.KeyID, 1, DisableKeyInput{
		OperatorReference: "change-disable", IdempotencyKey: "disable-one",
	})
	if err != nil || disabled.Versions[0].State != StateDisabled {
		t.Fatalf("disable failed: %#v, %v", disabled, err)
	}
	first := inventory(created.KeyID, 1, "inventory-1", fixture.now)
	invalidInventories := []struct {
		name      string
		inventory InventoryEvidence
		code      string
	}{
		{name: "missing", inventory: InventoryEvidence{}, code: "key_deletion_inventory_required"},
		{name: "stale", inventory: inventory(created.KeyID, 1, "stale", fixture.now.Add(-3*time.Hour)), code: "key_deletion_inventory_required"},
		{name: "mismatched", inventory: inventory(uuid.NewString(), 1, "mismatched", fixture.now), code: "key_deletion_inventory_required"},
		{name: "nonzero", inventory: func() InventoryEvidence {
			value := inventory(created.KeyID, 1, "nonzero", fixture.now)
			value.ResourceCount = 1
			return value
		}(), code: "key_deletion_inventory_nonzero"},
	}
	for _, test := range invalidInventories {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.service.ScheduleDeletion(context.Background(), disabler, created.KeyID, 1, ScheduleDeletionInput{
				OperatorReference: "change-delete", DestroyAfter: fixture.now.Add(7 * 24 * time.Hour),
				Inventory: test.inventory, IdempotencyKey: "invalid-" + test.name,
			})
			if errorCode(err) != test.code {
				t.Fatalf("inventory error = %v, want %s", err, test.code)
			}
		})
	}
	if _, err := fixture.service.ScheduleDeletion(context.Background(), disabler, created.KeyID, 1, ScheduleDeletionInput{
		OperatorReference: "change-delete", DestroyAfter: fixture.now.Add(6 * 24 * time.Hour), Inventory: first, IdempotencyKey: "schedule-too-soon",
	}); err == nil {
		t.Fatal("short deletion delay was accepted")
	}
	scheduled, err := fixture.service.ScheduleDeletion(context.Background(), disabler, created.KeyID, 1, ScheduleDeletionInput{
		OperatorReference: "change-delete", DestroyAfter: fixture.now.Add(7 * 24 * time.Hour), Inventory: first, IdempotencyKey: "schedule-one",
	})
	if err != nil || scheduled.Versions[0].State != StatePendingDeletion {
		t.Fatalf("schedule deletion failed: %#v, %v", scheduled, err)
	}
	approver := Actor{Identity: "approver-b", Role: RoleDeletionApprover}
	if _, err := fixture.service.DestroyKey(context.Background(), approver, created.KeyID, 1, DestroyKeyInput{
		OperatorReference: "approve-delete", Inventory: first, IdempotencyKey: "destroy-early",
	}); err == nil {
		t.Fatal("early destruction was accepted")
	}
	fixture.now = fixture.now.Add(7*24*time.Hour + time.Minute)
	second := inventory(created.KeyID, 1, "inventory-2", fixture.now)
	destroyed, err := fixture.service.DestroyKey(context.Background(), approver, created.KeyID, 1, DestroyKeyInput{
		OperatorReference: "approve-delete", Inventory: second, IdempotencyKey: "destroy-one",
	})
	if err != nil || destroyed.Versions[0].State != StateDestroyed || destroyed.Versions[0].DestroyedAt == nil {
		t.Fatalf("destruction failed: %#v, %v", destroyed, err)
	}
	var stored KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored.SealedMaterial) != 0 || stored.SealKeyID != "" {
		t.Fatal("destroyed key retained cryptographic material")
	}
	var destructionReceipt DestructionReceipt
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&destructionReceipt).Error; err != nil || destructionReceipt.ReceiptDigest == "" {
		t.Fatalf("missing destruction receipt: %#v, %v", destructionReceipt, err)
	}
	if err := fixture.db.Model(&DestructionReceipt{}).Where("id = ?", destructionReceipt.ID).
		Update("destroyed_by", "changed").Error; err == nil {
		t.Fatal("immutable destruction receipt accepted mutation")
	}
	if _, err := fixture.service.Unwrap(context.Background(), Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}, created.KeyID, 1, []byte("ciphertext"), []byte("aad")); errorCode(err) != "key_not_decryptable" {
		t.Fatalf("destroyed key remained decryptable: %v", err)
	}
}

func TestDeletionCanBeCancelledOnlyToDisabled(t *testing.T) {
	fixture := newServiceFixture(t, 0x46)
	created := fixture.createKey("create-cancel")
	disabler := Actor{Identity: "disabler-a", Role: RoleKeyDisabler}
	_, err := fixture.service.DisableKey(context.Background(), disabler, created.KeyID, 1, DisableKeyInput{OperatorReference: "disable", IdempotencyKey: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.ScheduleDeletion(context.Background(), disabler, created.KeyID, 1, ScheduleDeletionInput{
		OperatorReference: "delete", DestroyAfter: fixture.now.Add(8 * 24 * time.Hour), Inventory: inventory(created.KeyID, 1, "first", fixture.now), IdempotencyKey: "schedule",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := fixture.service.CancelDeletion(context.Background(), disabler, created.KeyID, 1, CancelDeletionInput{
		OperatorReference: "cancel", IdempotencyKey: "cancel",
	})
	if err != nil || cancelled.Versions[0].State != StateDisabled || cancelled.ActiveVersion != 0 {
		t.Fatalf("unexpected cancellation: %#v, %v", cancelled, err)
	}
}

func TestIdempotencyIsDeterministicUnderConcurrency(t *testing.T) {
	fixture := newServiceFixture(t, 0x47)
	const callers = 8
	results := make(chan KeyDescription, callers)
	errorsChannel := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := fixture.service.CreateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, CreateKeyInput{
				Name: "same", IdempotencyKey: "concurrent-create",
			})
			results <- result
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	var keyID string
	for result := range results {
		if keyID == "" {
			keyID = result.KeyID
		}
		if result.KeyID != keyID {
			t.Fatalf("idempotent create returned different keys: %s, %s", keyID, result.KeyID)
		}
	}
	var keyCount, versionCount int64
	_ = fixture.db.Model(&LogicalKey{}).Count(&keyCount).Error
	_ = fixture.db.Model(&KeyVersion{}).Count(&versionCount).Error
	if keyCount != 1 || versionCount != 1 {
		t.Fatalf("idempotency created %d keys and %d versions", keyCount, versionCount)
	}
}

func TestConcurrentRotationsPreserveExactlyOneActiveVersion(t *testing.T) {
	fixture := newServiceFixture(t, 0x74)
	created := fixture.createKey("concurrent-rotate-create")
	const rotations = 6
	var wait sync.WaitGroup
	errorsChannel := make(chan error, rotations)
	for index := range rotations {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := fixture.service.RotateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID, RotateKeyInput{
				OperatorReference: "concurrent-rotation", IdempotencyKey: fmt.Sprintf("rotate-%d", index),
			})
			errorsChannel <- err
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	description, err := fixture.service.DescribeKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	var active int
	for _, version := range description.Versions {
		if version.State == StateActive {
			active++
		} else if version.State != StateDecryptOnly {
			t.Fatalf("unexpected concurrent rotation state: %#v", version)
		}
	}
	if active != 1 || len(description.Versions) != rotations+1 || description.ActiveVersion != rotations+1 {
		t.Fatalf("concurrent rotation invariant failed: %#v", description)
	}
}

func TestMissingSealKeyAndCorruptedManagedMaterialFailClosed(t *testing.T) {
	fixture := newServiceFixture(t, 0x75)
	created := fixture.createKey("missing-seal-create")
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	otherSeals, _ := NewSealKeyring(bytes.Repeat([]byte{0x76}, 32))
	otherService, _ := NewService(fixture.db, otherSeals, ServiceOptions{Now: func() time.Time { return fixture.now }})
	if _, err := otherService.Wrap(context.Background(), cryptor, created.KeyID, 1, bytes.Repeat([]byte{0x22}, 32), []byte("aad")); errorCode(err) != "kms_unavailable" {
		t.Fatalf("missing seal key did not fail closed: %v", err)
	}
	if err := fixture.db.Model(&KeyVersion{}).Where("key_id = ? AND version = 1", created.KeyID).
		Update("sealed_material", []byte("corrupted")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Wrap(context.Background(), cryptor, created.KeyID, 1, bytes.Repeat([]byte{0x22}, 32), []byte("aad")); errorCode(err) != "kms_unavailable" {
		t.Fatalf("corrupted managed material did not fail closed: %v", err)
	}
}

func TestAuditFailureRollsBackLifecycleMutation(t *testing.T) {
	fixture := newServiceFixture(t, 0x77)
	created := fixture.createKey("audit-rollback-create")
	if err := fixture.db.Exec(`CREATE TRIGGER kms_test_reject_audit BEFORE INSERT ON kms_audit_events BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.RotateKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID, RotateKeyInput{
		OperatorReference: "audit-failure", IdempotencyKey: "audit-failure-rotate",
	})
	if errorCode(err) != "kms_unavailable" {
		t.Fatalf("expected audit failure, got %v", err)
	}
	if err := fixture.db.Exec(`DROP TRIGGER kms_test_reject_audit`).Error; err != nil {
		t.Fatal(err)
	}
	description, err := fixture.service.DescribeKey(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, created.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if description.ActiveVersion != 1 || len(description.Versions) != 1 || description.Versions[0].State != StateActive {
		t.Fatalf("audit failure left a partial rotation: %#v", description)
	}
}

func TestResealChangesOnlySealedRepresentationAndWritesImmutableReceipt(t *testing.T) {
	fixture := newServiceFixture(t, 0x48)
	created := fixture.createKey("create-reseal")
	var before KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	newSeals, err := NewSealKeyring(bytes.Repeat([]byte{0x49}, 32), bytes.Repeat([]byte{0x48}, 32))
	if err != nil {
		t.Fatal(err)
	}
	resealing, err := NewService(fixture.db, newSeals, ServiceOptions{
		Now: func() time.Time { return fixture.now }, MinimumDeleteDelay: 7 * 24 * time.Hour, InventoryMaximumAge: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := resealing.ResealAll(context.Background(), Actor{Identity: "manager-a", Role: RoleKeyManager}, "seal-rotation-1")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.VersionCount != 1 || receipt.NewSealKeyID != newSeals.PrimaryID() || receipt.ReceiptDigest == "" {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	var run ResealRun
	if err := fixture.db.Where("id = ?", receipt.RunID).Take(&run).Error; err != nil || run.VersionCount != 1 {
		t.Fatalf("missing reseal run: %#v, %v", run, err)
	}
	var after KeyVersion
	if err := fixture.db.Where("key_id = ? AND version = 1", created.KeyID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before.SealedMaterial, after.SealedMaterial) || after.SealKeyID != newSeals.PrimaryID() || before.KeyID != after.KeyID || before.Version != after.Version {
		t.Fatal("reseal did not preserve managed key identity")
	}
	cryptor := Actor{Identity: "control-plane-a", Role: RoleControlPlaneCryptor}
	wrapped, err := resealing.Wrap(context.Background(), cryptor, created.KeyID, 1, bytes.Repeat([]byte{0x11}, 32), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resealing.Unwrap(context.Background(), cryptor, created.KeyID, 1, wrapped.WrappedDataKey, []byte("aad")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ResealReceipt{}).Where("id = ?", receipt.ID).Update("version_count", 99).Error; err == nil {
		t.Fatal("immutable reseal receipt accepted mutation")
	}
}

func TestParseVersionedKeyIDIsExact(t *testing.T) {
	keyID := uuid.NewString()
	parsedID, version, err := ParseVersionedKeyID(VersionedKeyID(keyID, 12))
	if err != nil || parsedID != keyID || version != 12 {
		t.Fatalf("parse = %s, %d, %v", parsedID, version, err)
	}
	for _, invalid := range []string{keyID, keyID + "/versions/0", keyID + "/versions/1/extra", "not-a-uuid/versions/1"} {
		if _, _, err := ParseVersionedKeyID(invalid); err == nil {
			t.Fatalf("accepted invalid key identity %q", invalid)
		}
	}
}

func inventory(keyID string, version int64, evidenceID string, completedAt time.Time) InventoryEvidence {
	return InventoryEvidence{
		EvidenceID: evidenceID, DeploymentIdentity: "deployment-a", DatabaseIdentity: "database-a",
		KeyID: keyID, Version: version, ResourceCount: 0,
		Digest: hex.EncodeToString(bytes.Repeat([]byte{byte(len(evidenceID) + 1)}, 32)), CompletedAt: completedAt,
	}
}

func errorCode(err error) string {
	var typed *ServiceError
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
