package platform

import (
	"fmt"
	"sort"
	"strings"
)

type DeploymentProfile string

const (
	ProfilePersonal   DeploymentProfile = "personal"
	ProfileSingleNode DeploymentProfile = "single-node"
	ProfileEnterprise DeploymentProfile = "enterprise"
)

type MetadataStore string

const (
	MetadataSQLite   MetadataStore = "sqlite"
	MetadataPostgres MetadataStore = "postgresql"
)

type ArtifactStore string

const (
	ArtifactLocal ArtifactStore = "local"
	ArtifactMinIO ArtifactStore = "minio"
	ArtifactS3    ArtifactStore = "s3"
)

type QueueDriver string

const (
	QueueInProcess      QueueDriver = "in-process"
	QueuePostgresOutbox QueueDriver = "postgres-outbox"
	QueueExternal       QueueDriver = "external"
)

type ExecutionTargetKind string

const (
	TargetLocal      ExecutionTargetKind = "local"
	TargetSSH        ExecutionTargetKind = "ssh"
	TargetDocker     ExecutionTargetKind = "docker"
	TargetKubernetes ExecutionTargetKind = "kubernetes"
)

var executionTargetKinds = []ExecutionTargetKind{
	TargetLocal,
	TargetSSH,
	TargetDocker,
	TargetKubernetes,
}

type Config struct {
	Profile              DeploymentProfile
	MetadataStore        MetadataStore
	ArtifactStore        ArtifactStore
	QueueDriver          QueueDriver
	ControlPlaneReplicas int
	LeaseEnabled         bool
	FencingEnabled       bool
}

type PublicProfile struct {
	Profile                  DeploymentProfile     `json:"profile"`
	MetadataStore            MetadataStore         `json:"metadataStore"`
	ArtifactStore            ArtifactStore         `json:"artifactStore"`
	QueueDriver              QueueDriver           `json:"queueDriver"`
	ControlPlaneReplicas     int                   `json:"controlPlaneReplicas"`
	HighAvailability         bool                  `json:"highAvailability"`
	LeaseEnabled             bool                  `json:"leaseEnabled"`
	FencingEnabled           bool                  `json:"fencingEnabled"`
	ExecutionTargetKinds     []ExecutionTargetKind `json:"executionTargetKinds"`
	ArtifactPayloadMigration bool                  `json:"artifactPayloadMigration"`
	MetadataExportImport     bool                  `json:"metadataExportImport"`
}

func ParseDeploymentProfile(value string) (DeploymentProfile, error) {
	normalized := DeploymentProfile(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case ProfilePersonal, ProfileSingleNode, ProfileEnterprise:
		return normalized, nil
	default:
		return "", fmt.Errorf("deployment profile must be personal, single-node, or enterprise")
	}
}

func ParseMetadataStore(value string) (MetadataStore, error) {
	normalized := MetadataStore(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case MetadataSQLite, MetadataPostgres:
		return normalized, nil
	default:
		return "", fmt.Errorf("metadata store must be sqlite or postgresql")
	}
}

func ParseArtifactStore(value string) (ArtifactStore, error) {
	normalized := ArtifactStore(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case ArtifactLocal, ArtifactMinIO, ArtifactS3:
		return normalized, nil
	default:
		return "", fmt.Errorf("artifact store must be local, minio, or s3")
	}
}

func ParseQueueDriver(value string) (QueueDriver, error) {
	normalized := QueueDriver(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case QueueInProcess, QueuePostgresOutbox, QueueExternal:
		return normalized, nil
	default:
		return "", fmt.Errorf("queue driver must be in-process, postgres-outbox, or external")
	}
}

func ParseExecutionTargetKind(value string) (ExecutionTargetKind, error) {
	normalized := ExecutionTargetKind(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case TargetLocal, TargetSSH, TargetDocker, TargetKubernetes:
		return normalized, nil
	default:
		return "", fmt.Errorf("execution target kind must be local, ssh, docker, or kubernetes")
	}
}

func Defaults(profile DeploymentProfile) (Config, error) {
	switch profile {
	case ProfilePersonal:
		return Config{
			Profile: profile, MetadataStore: MetadataSQLite, ArtifactStore: ArtifactLocal,
			QueueDriver: QueueInProcess, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: true,
		}, nil
	case ProfileSingleNode:
		return Config{
			Profile: profile, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactMinIO,
			QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 1, LeaseEnabled: true, FencingEnabled: true,
		}, nil
	case ProfileEnterprise:
		return Config{
			Profile: profile, MetadataStore: MetadataPostgres, ArtifactStore: ArtifactS3,
			QueueDriver: QueuePostgresOutbox, ControlPlaneReplicas: 2, LeaseEnabled: true, FencingEnabled: true,
		}, nil
	default:
		return Config{}, fmt.Errorf("invalid deployment profile %q", profile)
	}
}

func (c Config) Validate() error {
	if _, err := ParseDeploymentProfile(string(c.Profile)); err != nil {
		return err
	}
	if _, err := ParseMetadataStore(string(c.MetadataStore)); err != nil {
		return err
	}
	if _, err := ParseArtifactStore(string(c.ArtifactStore)); err != nil {
		return err
	}
	if _, err := ParseQueueDriver(string(c.QueueDriver)); err != nil {
		return err
	}
	if c.ControlPlaneReplicas < 1 {
		return fmt.Errorf("control-plane replicas must be at least one")
	}
	if c.MetadataStore == MetadataSQLite && c.ControlPlaneReplicas != 1 {
		return fmt.Errorf("sqlite metadata store requires exactly one control-plane replica")
	}
	if c.ArtifactStore == ArtifactLocal && c.ControlPlaneReplicas != 1 {
		return fmt.Errorf("local artifact store requires exactly one control-plane replica")
	}
	if c.QueueDriver == QueueInProcess && c.ControlPlaneReplicas != 1 {
		return fmt.Errorf("in-process queue requires exactly one control-plane replica")
	}
	if !c.LeaseEnabled || !c.FencingEnabled {
		return fmt.Errorf("the v2 worker protocol requires both execution leases and generation fencing")
	}

	switch c.Profile {
	case ProfilePersonal:
		if c.MetadataStore != MetadataSQLite || c.ArtifactStore != ArtifactLocal || c.QueueDriver != QueueInProcess || c.ControlPlaneReplicas != 1 {
			return fmt.Errorf("personal profile requires sqlite metadata, local artifacts, in-process queue, and one replica")
		}
	case ProfileSingleNode:
		if c.MetadataStore != MetadataPostgres || c.ControlPlaneReplicas != 1 {
			return fmt.Errorf("single-node profile requires postgresql metadata and one replica")
		}
		if c.ArtifactStore != ArtifactMinIO && c.ArtifactStore != ArtifactS3 {
			return fmt.Errorf("single-node profile requires minio or s3 artifacts")
		}
		if c.QueueDriver != QueuePostgresOutbox {
			return fmt.Errorf("single-node profile requires the postgresql outbox queue")
		}
	case ProfileEnterprise:
		if c.MetadataStore != MetadataPostgres || c.ArtifactStore != ArtifactS3 || c.ControlPlaneReplicas < 2 {
			return fmt.Errorf("enterprise profile requires postgresql metadata, s3 artifacts, and multiple replicas")
		}
		if c.QueueDriver != QueuePostgresOutbox && c.QueueDriver != QueueExternal {
			return fmt.Errorf("enterprise profile requires the postgresql outbox or an external queue")
		}
	}
	return nil
}

func (c Config) Public() PublicProfile {
	kinds := append([]ExecutionTargetKind(nil), executionTargetKinds...)
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return PublicProfile{
		Profile: c.Profile, MetadataStore: c.MetadataStore, ArtifactStore: c.ArtifactStore,
		QueueDriver: c.QueueDriver, ControlPlaneReplicas: c.ControlPlaneReplicas,
		HighAvailability: c.ControlPlaneReplicas > 1, LeaseEnabled: c.LeaseEnabled,
		FencingEnabled: c.FencingEnabled, ExecutionTargetKinds: kinds,
		ArtifactPayloadMigration: true, MetadataExportImport: true,
	}
}

func IsRemoteTarget(kind ExecutionTargetKind) bool {
	return kind == TargetSSH || kind == TargetDocker || kind == TargetKubernetes
}
