package tracing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceparentRoundTrip(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	ctx, span := otel.Tracer("test").Start(context.Background(), "parent")
	traceparent := TraceparentFromContext(ctx)
	span.End()
	if len(traceparent) != 55 {
		t.Fatalf("traceparent = %q", traceparent)
	}
	remote := trace.SpanContextFromContext(ContextWithRemoteParent(context.Background(), traceparent))
	if !remote.IsValid() || !remote.IsRemote() || remote.TraceID() != trace.SpanContextFromContext(ctx).TraceID() {
		t.Fatalf("remote SpanContext = %#v", remote)
	}
}

func TestSampleRatioRejectsInvalidValue(t *testing.T) {
	t.Setenv(sampleRatioEnvironment, "1.5")
	if _, err := sampleRatio(); err == nil {
		t.Fatal("invalid trace sample ratio was accepted")
	}
}

func TestExporterSettingsFailClosedForUnsafeEndpointsAndCredentials(t *testing.T) {
	tests := []struct {
		name    string
		policy  ExportPolicy
		values  map[string]string
		wantErr string
	}{
		{
			name:    "relative endpoint",
			policy:  ExportPolicyDevelopment,
			values:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"},
			wantErr: "absolute HTTP(S)",
		},
		{
			name:    "embedded credentials",
			policy:  ExportPolicyDevelopment,
			values:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://token@collector.example.test"},
			wantErr: "must not contain credentials",
		},
		{
			name:   "authentication header over plaintext",
			policy: ExportPolicyDevelopment,
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example.test:4318",
				"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=secret",
			},
			wantErr: "headers require HTTPS",
		},
		{
			name:   "unsupported protocol",
			policy: ExportPolicyDevelopment,
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
			},
			wantErr: "protocol must be http/protobuf",
		},
		{
			name:   "insecure transport override",
			policy: ExportPolicyEnterprise,
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test",
				"OTEL_EXPORTER_OTLP_INSECURE": "true",
			},
			wantErr: "insecure transport override is forbidden",
		},
		{
			name:   "incomplete client identity",
			policy: ExportPolicyDevelopment,
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":           "https://collector.example.test",
				"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": "/missing/client.crt",
			},
			wantErr: "certificate and key must be configured together",
		},
		{
			name:    "enterprise plaintext",
			policy:  ExportPolicyEnterprise,
			values:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example.test:4318"},
			wantErr: "requires HTTPS",
		},
		{
			name:    "enterprise missing mtls",
			policy:  ExportPolicyEnterprise,
			values:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test"},
			wantErr: "requires an mTLS client certificate and key",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			for name, value := range test.values {
				t.Setenv(name, value)
			}
			_, err := exporterSettingsFromEnvironment(test.policy)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("exporterSettingsFromEnvironment() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestEnterpriseExporterRequiresBoundedRegionRetentionAndAbsoluteMTLSFiles(t *testing.T) {
	clearExporterEnvironment(t)
	certificate := filepath.Join(t.TempDir(), "client.crt")
	key := filepath.Join(t.TempDir(), "client.key")
	for path, content := range map[string]string{certificate: "certificate", key: "private-key"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://collector.eu-west.example.test/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", certificate)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY", key)
	t.Setenv(collectorRegionEnvironment, "EU-WEST-1")
	t.Setenv(retentionDaysEnvironment, "30")
	settings, err := exporterSettingsFromEnvironment(ExportPolicyEnterprise)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.configured || settings.endpoint.Scheme != "https" ||
		settings.region != "eu-west-1" || settings.retentionDays != 30 {
		t.Fatalf("enterprise exporter settings = %#v", settings)
	}
}

func TestEnterpriseExporterRejectsMissingOrUnboundedDataPolicy(t *testing.T) {
	clearExporterEnvironment(t)
	certificate := filepath.Join(t.TempDir(), "client.crt")
	key := filepath.Join(t.TempDir(), "client.key")
	for _, path := range []string{certificate, key} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.test")
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", certificate)
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", key)
	if _, err := exporterSettingsFromEnvironment(ExportPolicyEnterprise); err == nil ||
		!strings.Contains(err.Error(), collectorRegionEnvironment) {
		t.Fatalf("missing Region error = %v", err)
	}
	t.Setenv(collectorRegionEnvironment, "us-east-1")
	t.Setenv(retentionDaysEnvironment, "365")
	if _, err := exporterSettingsFromEnvironment(ExportPolicyEnterprise); err == nil ||
		!strings.Contains(err.Error(), "between 1 and 90") {
		t.Fatalf("unbounded retention error = %v", err)
	}
}

func TestEnterpriseWorkerExporterRequiresCredentiallessHTTPSOrLoopbackRelay(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{
			name: "credentialless HTTPS",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test/v1/traces",
			},
		},
		{
			name: "IPv4 loopback relay",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			},
		},
		{
			name: "IPv6 loopback relay",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://[::1]:4318",
			},
		},
		{
			name: "remote HTTP",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.example.test:4318",
			},
			wantErr: "explicit loopback relay",
		},
		{
			name: "DNS localhost",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
			},
			wantErr: "explicit loopback relay",
		},
		{
			name: "loopback without port",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1",
			},
			wantErr: "explicit loopback relay",
		},
		{
			name: "Header credential",
			values: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.test",
				"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=secret",
			},
			wantErr: "forbids authentication Headers",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			for name, value := range test.values {
				t.Setenv(name, value)
			}
			t.Setenv(collectorRegionEnvironment, "eu-west-1")
			t.Setenv(retentionDaysEnvironment, "30")
			settings, err := exporterSettingsFromEnvironment(ExportPolicyEnterpriseWorker)
			if test.wantErr == "" {
				if err != nil || !settings.configured {
					t.Fatalf("worker exporter settings = %#v, error = %v", settings, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("worker exporter error = %v, want %q", err, test.wantErr)
			}
		})
	}

	t.Run("client identity", func(t *testing.T) {
		clearExporterEnvironment(t)
		directory := t.TempDir()
		certificate := filepath.Join(directory, "client.crt")
		key := filepath.Join(directory, "client.key")
		if err := os.WriteFile(certificate, []byte("certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.test")
		t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", certificate)
		t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", key)
		t.Setenv(collectorRegionEnvironment, "eu-west-1")
		t.Setenv(retentionDaysEnvironment, "30")
		if _, err := exporterSettingsFromEnvironment(ExportPolicyEnterpriseWorker); err == nil ||
			!strings.Contains(err.Error(), "forbids client identity files") {
			t.Fatalf("worker client identity error = %v", err)
		}
	})
}

func clearExporterEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		collectorRegionEnvironment,
		retentionDaysEnvironment,
	} {
		t.Setenv(name, "")
	}
}
