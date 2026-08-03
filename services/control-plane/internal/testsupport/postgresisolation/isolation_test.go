package postgresisolation

import (
	"net/url"
	"testing"
)

func TestWithSearchPathPreservesConnectionOptionsAndReplacesCallerPath(t *testing.T) {
	isolateURL, err := withSearchPath(
		"postgres://user:secret@localhost:5432/synara?sslmode=disable&search_path=public",
		"synara_test_exact",
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(isolateURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("sslmode") != "disable" {
		t.Fatalf("sslmode = %q", parsed.Query().Get("sslmode"))
	}
	if parsed.Query().Get("search_path") != "synara_test_exact,public" {
		t.Fatalf("search_path = %q", parsed.Query().Get("search_path"))
	}
}

func TestWithSearchPathRejectsNonPostgresURL(t *testing.T) {
	if _, err := withSearchPath("sqlite:///tmp/synara.db", "synara_test_exact"); err == nil {
		t.Fatal("non-PostgreSQL URL was accepted")
	}
}
