package postgresisolation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	isolationMu      sync.Mutex
	isolationSchemas = map[string]string{}
)

// URL creates a test-scoped PostgreSQL schema and returns a connection URL
// whose search_path writes to that schema while retaining public for shared
// extension functions such as pgcrypto.digest. Repeated calls from the same
// test process and test name resolve to the same schema so multi-connection
// concurrency fixtures continue to share one isolated database boundary.
func URL(t testing.TB, databaseURL string) string {
	t.Helper()
	keyDigest := sha256.Sum256([]byte(databaseURL + "\x00" + t.Name()))
	isolationKey := hex.EncodeToString(keyDigest[:])
	isolationMu.Lock()
	defer isolationMu.Unlock()
	if schema, ok := isolationSchemas[isolationKey]; ok {
		isolatedURL, err := withSearchPath(databaseURL, schema)
		if err != nil {
			t.Fatalf("build reused isolated PostgreSQL test URL: %v", err)
		}
		return isolatedURL
	}
	schema := schemaName(t.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect PostgreSQL schema authority: %v", err)
	}
	if _, err := connection.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public"); err != nil {
		_ = connection.Close(ctx)
		t.Fatalf("prepare shared PostgreSQL pgcrypto extension: %v", err)
	}
	var extensionSchema string
	if err := connection.QueryRow(ctx, `
		SELECT namespace.nspname
		FROM pg_extension AS extension
		JOIN pg_namespace AS namespace ON namespace.oid = extension.extnamespace
		WHERE extension.extname = 'pgcrypto'
	`).Scan(&extensionSchema); err != nil {
		_ = connection.Close(ctx)
		t.Fatalf("read PostgreSQL pgcrypto extension schema: %v", err)
	}
	if extensionSchema != "public" {
		_ = connection.Close(ctx)
		t.Fatalf("PostgreSQL pgcrypto extension must be installed in public, got %q", extensionSchema)
	}
	if _, err := connection.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = connection.Close(ctx)
		t.Fatalf("create isolated PostgreSQL test schema: %v", err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatalf("close PostgreSQL schema authority: %v", err)
	}
	isolationSchemas[isolationKey] = schema

	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		cleanupConnection, cleanupErr := pgx.Connect(cleanupContext, databaseURL)
		if cleanupErr != nil {
			t.Errorf("connect PostgreSQL schema cleanup authority: %v", cleanupErr)
			return
		}
		defer func() { _ = cleanupConnection.Close(cleanupContext) }()
		if _, cleanupErr = cleanupConnection.Exec(
			cleanupContext,
			"DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE",
		); cleanupErr != nil {
			t.Errorf("drop isolated PostgreSQL test schema: %v", cleanupErr)
		}
	})

	isolatedURL, err := withSearchPath(databaseURL, schema)
	if err != nil {
		t.Fatalf("build isolated PostgreSQL test URL: %v", err)
	}
	return isolatedURL
}

func schemaName(testName string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", os.Getpid(), testName)))
	return "synara_test_" + hex.EncodeToString(digest[:12])
}

func withSearchPath(databaseURL string, schema string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", fmt.Errorf("unsupported PostgreSQL URL scheme %q", parsed.Scheme)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
