package kmsworker

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestPostgreSQLLifecycleAndImmutableEvidence(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_KMS_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_KMS_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db, err := OpenStore(ctx, StoreConfig{Driver: "postgres", DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := MigrateStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	seals, _ := NewSealKeyring(bytes.Repeat([]byte{0x72}, 32))
	service, err := NewService(db, seals, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateKey(ctx, Actor{Identity: "postgres-manager", Role: RoleKeyManager}, CreateKeyInput{
		Name: "postgres-key", IdempotencyKey: "postgres-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := service.RotateKey(ctx, Actor{Identity: "postgres-manager", Role: RoleKeyManager}, created.KeyID, RotateKeyInput{
		OperatorReference: "postgres-rotation", IdempotencyKey: "postgres-rotate",
	})
	if err != nil || rotated.ActiveVersion != 2 || rotated.Versions[0].State != StateDecryptOnly {
		t.Fatalf("unexpected PostgreSQL rotation: %#v, %v", rotated, err)
	}
	if err := db.Model(&AuditEvent{}).Where("key_id = ?", created.KeyID).Update("outcome", "changed").Error; err == nil {
		t.Fatal("PostgreSQL immutable audit trigger accepted an update")
	}
}
