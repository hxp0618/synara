package database

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestReadMigrationsRejectsDuplicateVersion(t *testing.T) {
	files := fstest.MapFS{
		"000107_runtime_isolation.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000107_desktop_enrollment.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
	}

	_, err := readMigrations(files)
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version 107") {
		t.Fatalf("readMigrations duplicate version error = %v", err)
	}
}
