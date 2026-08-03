package agentd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestLoadObservabilityEnvironmentAppliesTargetScopedAllowlist(t *testing.T) {
	cfg, path := observabilityEnvironmentFixture(t, platform.TargetDocker, strings.Join([]string{
		"# target-local OTLP configuration",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test/v1/traces",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_CERTIFICATE=${TARGET_DIR}/ca.crt",
		"SYNARA_OTEL_TRACE_SAMPLE_RATIO=0.25",
		"SYNARA_OTEL_COLLECTOR_REGION=eu-west-1",
		"SYNARA_OTEL_TRACE_RETENTION_DAYS=30",
	}, "\n"))
	t.Setenv(ObservabilityEnvironmentFileVariable, path)
	clearObservabilityEnvironment(t)
	if err := LoadObservabilityEnvironment(cfg); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "https://collector.example.test/v1/traces" {
		t.Fatalf("OTLP endpoint = %q", got)
	}
	if got := os.Getenv("SYNARA_OTEL_COLLECTOR_REGION"); got != "eu-west-1" {
		t.Fatalf("collector Region = %q", got)
	}
}

func TestLoadObservabilityEnvironmentRejectsUnsafeAuthorityAndSyntax(t *testing.T) {
	tests := []struct {
		name       string
		targetKind platform.ExecutionTargetKind
		payload    string
		mutatePath func(*testing.T, Config, string) string
		want       string
	}{
		{
			name: "local worker", targetKind: platform.TargetLocal,
			payload: "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test\n",
			want:    "only valid for non-Local workers",
		},
		{
			name: "wrong Target path", targetKind: platform.TargetSSH,
			payload: "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test\n",
			mutatePath: func(t *testing.T, _ Config, path string) string {
				t.Helper()
				wrongDirectory := filepath.Join(filepath.Dir(filepath.Dir(path)), uuid.NewString())
				if err := os.Mkdir(wrongDirectory, 0o700); err != nil {
					t.Fatal(err)
				}
				wrongPath := filepath.Join(wrongDirectory, observabilityEnvironmentFilename)
				if err := os.WriteFile(wrongPath, []byte("OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return wrongPath
			},
			want: "Target-scoped",
		},
		{
			name: "header secret", targetKind: platform.TargetDocker,
			payload: "OTEL_EXPORTER_OTLP_HEADERS=authorization=secret\n",
			want:    "forbidden key OTEL_EXPORTER_OTLP_HEADERS",
		},
		{
			name: "insecure override", targetKind: platform.TargetDocker,
			payload: "OTEL_EXPORTER_OTLP_INSECURE=false\n",
			want:    "forbidden key OTEL_EXPORTER_OTLP_INSECURE",
		},
		{
			name: "duplicate", targetKind: platform.TargetDocker,
			payload: "SYNARA_OTEL_COLLECTOR_REGION=eu-west-1\nSYNARA_OTEL_COLLECTOR_REGION=us-east-1\n",
			want:    "repeats SYNARA_OTEL_COLLECTOR_REGION",
		},
		{
			name: "CA path escape", targetKind: platform.TargetDocker,
			payload: "OTEL_EXPORTER_OTLP_CERTIFICATE=/data/ca.crt\n",
			want:    "must use the Target-local ca.crt path",
		},
		{
			name: "client identity", targetKind: platform.TargetDocker,
			payload: "OTEL_EXPORTER_OTLP_CLIENT_KEY=/data/client.key\n",
			want:    "forbidden key OTEL_EXPORTER_OTLP_CLIENT_KEY",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, path := observabilityEnvironmentFixture(t, test.targetKind, test.payload)
			if test.mutatePath != nil {
				path = test.mutatePath(t, cfg, path)
			}
			t.Setenv(ObservabilityEnvironmentFileVariable, path)
			clearObservabilityEnvironment(t)
			err := LoadObservabilityEnvironment(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadObservabilityEnvironment() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadObservabilityEnvironmentRejectsSymlinkWritableAndAmbientConflict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink semantics")
	}
	t.Run("symlink", func(t *testing.T) {
		cfg, path := observabilityEnvironmentFixture(t, platform.TargetSSH, "SYNARA_OTEL_COLLECTOR_REGION=eu-west-1\n")
		realPath := filepath.Join(filepath.Dir(path), "real.env")
		if err := os.Rename(path, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, path); err != nil {
			t.Fatal(err)
		}
		t.Setenv(ObservabilityEnvironmentFileVariable, path)
		clearObservabilityEnvironment(t)
		if err := LoadObservabilityEnvironment(cfg); err == nil || !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("symlink error = %v", err)
		}
	})
	t.Run("writable", func(t *testing.T) {
		cfg, path := observabilityEnvironmentFixture(t, platform.TargetDocker, "SYNARA_OTEL_COLLECTOR_REGION=eu-west-1\n")
		if err := os.Chmod(path, 0o622); err != nil {
			t.Fatal(err)
		}
		t.Setenv(ObservabilityEnvironmentFileVariable, path)
		clearObservabilityEnvironment(t)
		if err := LoadObservabilityEnvironment(cfg); err == nil || !strings.Contains(err.Error(), "world-writable") {
			t.Fatalf("writable error = %v", err)
		}
	})
	t.Run("ambient conflict", func(t *testing.T) {
		cfg, path := observabilityEnvironmentFixture(t, platform.TargetDocker, "SYNARA_OTEL_COLLECTOR_REGION=eu-west-1\n")
		t.Setenv(ObservabilityEnvironmentFileVariable, path)
		clearObservabilityEnvironment(t)
		t.Setenv("SYNARA_OTEL_COLLECTOR_REGION", "us-east-1")
		if err := LoadObservabilityEnvironment(cfg); err == nil || !strings.Contains(err.Error(), "conflicts with process") {
			t.Fatalf("ambient conflict error = %v", err)
		}
	})
	t.Run("ambient Header bypass", func(t *testing.T) {
		cfg, path := observabilityEnvironmentFixture(t, platform.TargetDocker, "SYNARA_OTEL_COLLECTOR_REGION=eu-west-1\n")
		t.Setenv(ObservabilityEnvironmentFileVariable, path)
		clearObservabilityEnvironment(t)
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=secret")
		if err := LoadObservabilityEnvironment(cfg); err == nil || !strings.Contains(err.Error(), "forbids ambient OTEL_EXPORTER_OTLP_HEADERS") {
			t.Fatalf("ambient Header error = %v", err)
		}
	})
}

func TestLoadObservabilityEnvironmentDoesNotPartiallyApplyInvalidFile(t *testing.T) {
	cfg, path := observabilityEnvironmentFixture(t, platform.TargetDocker, strings.Join([]string{
		"SYNARA_OTEL_COLLECTOR_REGION=eu-west-1",
		"OTEL_EXPORTER_OTLP_HEADERS=authorization=secret",
	}, "\n"))
	t.Setenv(ObservabilityEnvironmentFileVariable, path)
	clearObservabilityEnvironment(t)
	if err := LoadObservabilityEnvironment(cfg); err == nil {
		t.Fatal("invalid environment file was accepted")
	}
	if got := os.Getenv("SYNARA_OTEL_COLLECTOR_REGION"); got != "" {
		t.Fatalf("invalid file partially applied Region %q", got)
	}
}

func observabilityEnvironmentFixture(
	t *testing.T,
	targetKind platform.ExecutionTargetKind,
	payload string,
) (Config, string) {
	t.Helper()
	targetID := uuid.New()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, targetID.String())
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, observabilityEnvironmentFilename)
	payload = strings.ReplaceAll(payload, "${TARGET_DIR}", directory)
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{ExecutionTargetID: targetID, TargetKind: targetKind}, path
}

func clearObservabilityEnvironment(t *testing.T) {
	t.Helper()
	for name := range observabilityEnvironmentNames {
		t.Setenv(name, "")
	}
	for _, name := range observabilityForbiddenAmbientNames {
		t.Setenv(name, "")
	}
}
