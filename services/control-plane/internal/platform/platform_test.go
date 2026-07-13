package platform

import (
	"strings"
	"testing"
)

func TestCompatibilityMatrix(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "sqlite single replica allowed", config: Config{Profile: ProfilePersonal, MetadataStore: MetadataSQLite, ArtifactStore: ArtifactLocal, QueueDriver: QueueInProcess, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: true}},
		{name: "sqlite multi replica rejected", config: Config{Profile: ProfilePersonal, MetadataStore: MetadataSQLite, ArtifactStore: ArtifactLocal, QueueDriver: QueueInProcess, ControlPlaneReplicas: 2, LeaseEnabled: true, FencingEnabled: true}, wantErr: true},
		{name: "local artifact single node allowed", config: Config{Profile: ProfilePersonal, MetadataStore: MetadataSQLite, ArtifactStore: ArtifactLocal, QueueDriver: QueueInProcess, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: true}},
		{name: "local artifact multi node rejected", config: Config{Profile: ProfileEnterprise, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactLocal, QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 2, LeaseEnabled: true, FencingEnabled: true}, wantErr: true},
		{name: "in process queue multi replica rejected", config: Config{Profile: ProfileEnterprise, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactS3, QueueDriver: QueueInProcess, ControlPlaneReplicas: 2, LeaseEnabled: true, FencingEnabled: true}, wantErr: true},
		{name: "single node postgres allowed", config: Config{Profile: ProfileSingleNode, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactMinIO, QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: true}},
		{name: "enterprise postgres outbox allowed", config: Config{Profile: ProfileEnterprise, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactS3, QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 3, LeaseEnabled: true, FencingEnabled: true}},
		{name: "enterprise external queue allowed", config: Config{Profile: ProfileEnterprise, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactS3, QueueDriver: QueueExternal, ControlPlaneReplicas: 3, LeaseEnabled: true, FencingEnabled: true}},
		{name: "lease disabled rejected", config: Config{Profile: ProfileSingleNode, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactMinIO, QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 1, LeaseEnabled: false, FencingEnabled: true}, wantErr: true},
		{name: "fencing disabled rejected", config: Config{Profile: ProfileSingleNode, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactMinIO, QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: false}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestPostgresSupportsEveryExecutionTargetKind(t *testing.T) {
	config, err := Defaults(ProfileSingleNode)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"local", "ssh", "docker", "kubernetes"} {
		if _, err := ParseExecutionTargetKind(value); err != nil {
			t.Fatalf("postgres profile rejected target kind %q: %v", value, err)
		}
	}
}

func TestWorkerProtocolValidationNamesCurrentVersion(t *testing.T) {
	config, err := Defaults(ProfileSingleNode)
	if err != nil {
		t.Fatal(err)
	}
	config.LeaseEnabled = false
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "v2 worker protocol") {
		t.Fatalf("expected current Worker Protocol version in validation error, got %v", err)
	}
}
