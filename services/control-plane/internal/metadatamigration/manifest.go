package metadatamigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const SchemaVersion = 1

type ArtifactBoundary struct {
	SchemaVersion             int                    `json:"schemaVersion"`
	PayloadsIncluded          bool                   `json:"payloadsIncluded"`
	PayloadMigrationSupported bool                   `json:"payloadMigrationSupported"`
	Entries                   []ArtifactPayloadEntry `json:"entries"`
}

type ArtifactPayloadEntry struct {
	ArtifactID      uuid.UUID `json:"artifactId"`
	SourceObjectKey string    `json:"sourceObjectKey"`
	SizeBytes       int64     `json:"sizeBytes"`
	SHA256          string    `json:"sha256"`
	ContentType     string    `json:"contentType"`
}

type WorkerArchive struct {
	ID                uuid.UUID      `json:"id"`
	ExecutionTargetID uuid.UUID      `json:"executionTargetId"`
	TargetKind        string         `json:"targetKind"`
	ClusterID         string         `json:"clusterId"`
	Namespace         string         `json:"namespace"`
	PodName           string         `json:"podName"`
	Version           string         `json:"version"`
	ProtocolVersion   int            `json:"protocolVersion"`
	Capabilities      map[string]any `json:"capabilities" gorm:"column:capabilities;serializer:json"`
	RegisteredAt      time.Time      `json:"registeredAt"`
	LastHeartbeatAt   time.Time      `json:"lastHeartbeatAt"`
}

type Data struct {
	Users                   []persistence.User                   `json:"users"`
	UserIdentities          []persistence.UserIdentity           `json:"userIdentities"`
	Tenants                 []persistence.Tenant                 `json:"tenants"`
	TenantMemberships       []persistence.TenantMembership       `json:"tenantMemberships"`
	Organizations           []persistence.Organization           `json:"organizations"`
	OrganizationMemberships []persistence.OrganizationMembership `json:"organizationMemberships"`
	TenantInvitations       []persistence.TenantInvitation       `json:"tenantInvitations"`
	Projects                []persistence.Project                `json:"projects"`
	ExecutionTargets        []persistence.ExecutionTarget        `json:"executionTargets"`
	AgentSessions           []persistence.AgentSession           `json:"agentSessions"`
	AgentTurns              []persistence.AgentTurn              `json:"agentTurns"`
	AgentExecutions         []persistence.AgentExecution         `json:"agentExecutions"`
	ArtifactRecords         []persistence.Artifact               `json:"artifactRecords"`
	SessionEvents           []persistence.SessionEvent           `json:"sessionEvents"`
	Automations             []persistence.Automation             `json:"automations"`
	Workers                 []WorkerArchive                      `json:"workers"`
	AuditLogs               []persistence.AuditLog               `json:"auditLogs"`
	OutboxMessages          []persistence.OutboxMessage          `json:"outboxMessages"`
}

type Manifest struct {
	SchemaVersion int                              `json:"schemaVersion"`
	ManifestID    string                           `json:"manifestId"`
	SourceProfile platform.DeploymentProfile       `json:"sourceProfile"`
	GeneratedAt   time.Time                        `json:"generatedAt"`
	Installation  persistence.PlatformInstallation `json:"installation"`
	Data          Data                             `json:"data"`
	Artifacts     ArtifactBoundary                 `json:"artifacts"`
}

type ImportReport struct {
	ManifestID               string `json:"manifestId"`
	Replayed                 bool   `json:"replayed"`
	ArtifactPayloadMigration string `json:"artifactPayloadMigration"`
}

func Export(ctx context.Context, db *gorm.DB, profile platform.DeploymentProfile) (Manifest, error) {
	if profile != platform.ProfilePersonal {
		return Manifest{}, fmt.Errorf("metadata export v1 requires source profile personal")
	}
	var unownedTargets int64
	if err := db.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
		Where("tenant_id IS NULL OR organization_id IS NULL").Count(&unownedTargets).Error; err != nil {
		return Manifest{}, fmt.Errorf("validate personal execution target ownership: %w", err)
	}
	if unownedTargets != 0 {
		return Manifest{}, fmt.Errorf("personal metadata contains execution targets without tenant and organization ownership")
	}
	var activeExecutions int64
	if err := db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("status IN ?", []string{"leased", "running", "waiting-for-approval", "recovering"}).Count(&activeExecutions).Error; err != nil {
		return Manifest{}, fmt.Errorf("check active executions: %w", err)
	}
	var leases int64
	if err := db.WithContext(ctx).Model(&persistence.WorkerLease{}).Count(&leases).Error; err != nil {
		return Manifest{}, fmt.Errorf("check worker leases: %w", err)
	}
	if activeExecutions != 0 || leases != 0 {
		return Manifest{}, fmt.Errorf("metadata export requires all executions to be quiesced and all leases released")
	}

	manifest := Manifest{
		SchemaVersion: SchemaVersion, ManifestID: uuid.NewString(), SourceProfile: profile,
		GeneratedAt: time.Now().UTC(),
		Artifacts: ArtifactBoundary{
			SchemaVersion: 1, PayloadsIncluded: false, PayloadMigrationSupported: true,
			Entries: []ArtifactPayloadEntry{},
		},
	}
	if err := db.WithContext(ctx).Where("key = ?", "control-plane").Take(&manifest.Installation).Error; err != nil {
		return Manifest{}, fmt.Errorf("load installation metadata: %w", err)
	}
	loaders := []func() error{
		func() error { return findAll(ctx, db, &manifest.Data.Users) },
		func() error { return findAll(ctx, db, &manifest.Data.UserIdentities) },
		func() error { return findAll(ctx, db, &manifest.Data.Tenants) },
		func() error { return findAll(ctx, db, &manifest.Data.TenantMemberships) },
		func() error { return findAll(ctx, db, &manifest.Data.Organizations) },
		func() error { return findAll(ctx, db, &manifest.Data.OrganizationMemberships) },
		func() error { return findAll(ctx, db, &manifest.Data.TenantInvitations) },
		func() error { return findAll(ctx, db, &manifest.Data.Projects) },
		func() error { return findAll(ctx, db, &manifest.Data.ExecutionTargets) },
		func() error { return findAll(ctx, db, &manifest.Data.AgentSessions) },
		func() error { return findAll(ctx, db, &manifest.Data.AgentTurns) },
		func() error { return findAll(ctx, db, &manifest.Data.AgentExecutions) },
		func() error { return findAll(ctx, db, &manifest.Data.ArtifactRecords) },
		func() error { return findAll(ctx, db, &manifest.Data.SessionEvents) },
		func() error { return findAll(ctx, db, &manifest.Data.Automations) },
		func() error { return findWorkers(ctx, db, &manifest.Data.Workers) },
		func() error { return findAll(ctx, db, &manifest.Data.AuditLogs) },
		func() error { return findAll(ctx, db, &manifest.Data.OutboxMessages) },
	}
	for _, load := range loaders {
		if err := load(); err != nil {
			return Manifest{}, err
		}
	}
	for index := range manifest.Data.ArtifactRecords {
		artifact := &manifest.Data.ArtifactRecords[index]
		if artifact.Status == "pending" || artifact.Status == "deleting" {
			return Manifest{}, fmt.Errorf("metadata export requires artifact %s to leave status %s", artifact.ID, artifact.Status)
		}
		artifact.UploadTokenHash = nil
		artifact.UploadExpiresAt = nil
		artifact.UploadObjectKey = nil
		if artifact.Status != "ready" || artifact.DeletedAt != nil {
			continue
		}
		if artifact.SizeBytes == nil || artifact.SHA256 == nil || artifact.ContentType == nil {
			return Manifest{}, fmt.Errorf("ready artifact %s is missing verified payload metadata", artifact.ID)
		}
		manifest.Artifacts.Entries = append(manifest.Artifacts.Entries, ArtifactPayloadEntry{
			ArtifactID: artifact.ID, SourceObjectKey: artifact.ObjectKey,
			SizeBytes: *artifact.SizeBytes, SHA256: *artifact.SHA256, ContentType: *artifact.ContentType,
		})
	}
	return manifest, nil
}

func Encode(manifest Manifest) ([]byte, error) {
	return json.MarshalIndent(manifest, "", "  ")
}

func Decode(encoded []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode metadata manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("decode metadata manifest: trailing JSON content")
	}
	return manifest, nil
}

func Import(ctx context.Context, db *gorm.DB, target platform.Config, manifest Manifest, encoded []byte) (ImportReport, error) {
	if target.MetadataStore != platform.MetadataPostgres || (target.Profile != platform.ProfileSingleNode && target.Profile != platform.ProfileEnterprise) {
		return ImportReport{}, fmt.Errorf("metadata import v1 requires a single-node or enterprise postgresql target")
	}
	if manifest.SchemaVersion != SchemaVersion {
		return ImportReport{}, fmt.Errorf("unsupported metadata manifest schema version %d", manifest.SchemaVersion)
	}
	if manifest.SourceProfile != platform.ProfilePersonal || manifest.Installation.Profile != string(platform.ProfilePersonal) {
		return ImportReport{}, fmt.Errorf("metadata manifest source profile must be personal")
	}
	if _, err := uuid.Parse(manifest.ManifestID); err != nil {
		return ImportReport{}, fmt.Errorf("metadata manifest id is invalid")
	}
	if manifest.Installation.InstallationID == "" {
		return ImportReport{}, fmt.Errorf("metadata manifest installation id is missing")
	}
	if err := validateArtifactBoundary(manifest); err != nil {
		return ImportReport{}, err
	}
	checksumBytes := sha256.Sum256(encoded)
	checksum := hex.EncodeToString(checksumBytes[:])
	report := ImportReport{
		ManifestID:               manifest.ManifestID,
		ArtifactPayloadMigration: "not_required",
	}
	if len(manifest.Artifacts.Entries) > 0 {
		report.ArtifactPayloadMigration = "pending_source_required"
	}

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing persistence.MetadataImport
		err := tx.Where("manifest_id = ?", manifest.ManifestID).Take(&existing).Error
		if err == nil {
			if existing.Checksum != checksum {
				return fmt.Errorf("manifest id was already imported with another checksum")
			}
			report.Replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var tenantCount int64
		if err := tx.Model(&persistence.Tenant{}).Count(&tenantCount).Error; err != nil {
			return err
		}
		if tenantCount != 0 {
			return fmt.Errorf("target metadata store must be empty for a v1 personal import")
		}
		var workerCount int64
		if err := tx.Model(&persistence.WorkerInstance{}).Count(&workerCount).Error; err != nil {
			return err
		}
		if workerCount != 0 {
			return fmt.Errorf("target metadata store has registered workers and is not safe to import")
		}
		if err := tx.Where("tenant_id IS NULL").Delete(&persistence.ExecutionTarget{}).Error; err != nil {
			return err
		}
		if err := tx.Where("key = ?", "control-plane").Delete(&persistence.PlatformInstallation{}).Error; err != nil {
			return err
		}

		installation := manifest.Installation
		installation.Profile = string(target.Profile)
		installation.UpdatedAt = time.Now().UTC()
		if err := tx.Create(&installation).Error; err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.Users); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.UserIdentities); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.Tenants); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.TenantMemberships); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.Organizations); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.OrganizationMemberships); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.TenantInvitations); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.Projects); err != nil {
			return err
		}
		if err := insertExecutionTargets(tx, manifest.Data.ExecutionTargets); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.AgentSessions); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.AgentTurns); err != nil {
			return err
		}
		if err := insertWorkers(tx, manifest.Data.Workers); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.AgentExecutions); err != nil {
			return err
		}
		if err := insertArtifacts(tx, manifest.Data.ArtifactRecords); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.SessionEvents); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.Automations); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.AuditLogs); err != nil {
			return err
		}
		if err := insertAll(tx, manifest.Data.OutboxMessages); err != nil {
			return err
		}
		return tx.Create(&persistence.MetadataImport{
			ManifestID: manifest.ManifestID, Checksum: checksum, ImportedAt: time.Now().UTC(),
		}).Error
	})
	if err != nil {
		return ImportReport{}, fmt.Errorf("import metadata manifest transactionally: %w", err)
	}
	return report, nil
}

func validateArtifactBoundary(manifest Manifest) error {
	boundary := manifest.Artifacts
	if boundary.SchemaVersion != 1 || boundary.PayloadsIncluded {
		return fmt.Errorf("unsupported artifact payload manifest boundary")
	}
	if len(boundary.Entries) > 0 && !boundary.PayloadMigrationSupported {
		return fmt.Errorf("artifact payload entries require payload migration support")
	}
	records := make(map[uuid.UUID]persistence.Artifact, len(manifest.Data.ArtifactRecords))
	for _, artifact := range manifest.Data.ArtifactRecords {
		if _, exists := records[artifact.ID]; exists {
			return fmt.Errorf("artifact metadata contains duplicate id %s", artifact.ID)
		}
		records[artifact.ID] = artifact
	}
	seen := make(map[uuid.UUID]struct{}, len(boundary.Entries))
	for _, entry := range boundary.Entries {
		if entry.ArtifactID == uuid.Nil || entry.SourceObjectKey == "" || entry.SizeBytes < 0 || len(entry.SHA256) != 64 || entry.ContentType == "" {
			return fmt.Errorf("artifact payload entry is invalid")
		}
		if _, exists := seen[entry.ArtifactID]; exists {
			return fmt.Errorf("artifact payload entry %s is duplicated", entry.ArtifactID)
		}
		seen[entry.ArtifactID] = struct{}{}
		artifact, exists := records[entry.ArtifactID]
		if !exists || artifact.Status != "ready" || artifact.DeletedAt != nil || artifact.ObjectKey != entry.SourceObjectKey ||
			artifact.SizeBytes == nil || *artifact.SizeBytes != entry.SizeBytes || artifact.SHA256 == nil || *artifact.SHA256 != entry.SHA256 ||
			artifact.ContentType == nil || *artifact.ContentType != entry.ContentType {
			return fmt.Errorf("artifact payload entry %s does not match ready artifact metadata", entry.ArtifactID)
		}
	}
	return nil
}

func findAll[T any](ctx context.Context, db *gorm.DB, target *[]T) error {
	if err := db.WithContext(ctx).Find(target).Error; err != nil {
		return fmt.Errorf("export metadata table: %w", err)
	}
	if *target == nil {
		*target = []T{}
	}
	return nil
}

func findWorkers(ctx context.Context, db *gorm.DB, target *[]WorkerArchive) error {
	if err := db.WithContext(ctx).Model(&persistence.WorkerInstance{}).Find(target).Error; err != nil {
		return fmt.Errorf("export worker archive: %w", err)
	}
	if *target == nil {
		*target = []WorkerArchive{}
	}
	return nil
}

func insertAll[T any](tx *gorm.DB, values []T) error {
	if len(values) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&values).Error
}

func insertExecutionTargets(tx *gorm.DB, values []persistence.ExecutionTarget) error {
	for index := range values {
		query := tx.Clauses(clause.OnConflict{DoNothing: true})
		if len(values[index].ConfigurationEncrypted) == 0 {
			query = query.Omit("ConfigurationEncrypted")
		}
		if err := query.Create(&values[index]).Error; err != nil {
			return err
		}
	}
	return nil
}

func insertArtifacts(tx *gorm.DB, values []persistence.Artifact) error {
	for index := range values {
		values[index].UploadTokenHash = nil
		values[index].UploadExpiresAt = nil
		values[index].UploadObjectKey = nil
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Omit("UploadTokenHash", "UploadExpiresAt", "UploadObjectKey").Create(&values[index]).Error; err != nil {
			return err
		}
	}
	return nil
}

func insertWorkers(tx *gorm.DB, values []WorkerArchive) error {
	for _, archived := range values {
		protocolVersion := archived.ProtocolVersion
		if protocolVersion <= 0 {
			protocolVersion = 1
		}
		hash := sha256.Sum256([]byte("migrated-worker:" + archived.ID.String()))
		worker := persistence.WorkerInstance{
			ID: archived.ID, ExecutionTargetID: archived.ExecutionTargetID, TargetKind: archived.TargetKind,
			ClusterID: archived.ClusterID, Namespace: archived.Namespace, PodName: archived.PodName,
			Version: archived.Version, ProtocolVersion: protocolVersion,
			Capabilities: archived.Capabilities, LeaseSupported: true,
			FencingSupported: true, AuthTokenHash: hash[:], Status: "terminated",
			RegisteredAt: archived.RegisteredAt, LastHeartbeatAt: archived.LastHeartbeatAt,
			TerminatedAt: pointerTime(time.Now().UTC()),
		}
		if err := tx.Create(&worker).Error; err != nil {
			return err
		}
	}
	return nil
}

func pointerTime(value time.Time) *time.Time { return &value }
