package metadatamigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type ArtifactPayloadMigrationReport struct {
	Total    int `json:"total"`
	Migrated int `json:"migrated"`
	Replayed int `json:"replayed"`
}

func ValidateArtifactPayloads(ctx context.Context, manifest Manifest, source artifacts.Store) error {
	if err := validateArtifactBoundary(manifest); err != nil {
		return err
	}
	for _, entry := range manifest.Artifacts.Entries {
		if err := validateObject(ctx, source, entry.SourceObjectKey, entry.SizeBytes, entry.SHA256); err != nil {
			return fmt.Errorf("validate source artifact %s: %w", entry.ArtifactID, err)
		}
	}
	return nil
}

func MigrateArtifactPayloads(
	ctx context.Context,
	db *gorm.DB,
	manifest Manifest,
	source artifacts.Store,
	destination artifacts.Store,
) (ArtifactPayloadMigrationReport, error) {
	if destination.IsLocal() {
		return ArtifactPayloadMigrationReport{}, fmt.Errorf("artifact payload migration destination must be MinIO or S3")
	}
	if err := ValidateArtifactPayloads(ctx, manifest, source); err != nil {
		return ArtifactPayloadMigrationReport{}, err
	}
	report := ArtifactPayloadMigrationReport{Total: len(manifest.Artifacts.Entries)}
	for _, entry := range manifest.Artifacts.Entries {
		destinationID := destination.Bucket() + "/" + entry.SourceObjectKey
		var previous persistence.ArtifactPayloadMigration
		err := db.WithContext(ctx).
			Where("artifact_id = ? AND destination = ?", entry.ArtifactID, destinationID).
			Take(&previous).Error
		if err == nil && previous.SourceSHA256 == entry.SHA256 {
			if objectErr := validateObject(ctx, destination, entry.SourceObjectKey, entry.SizeBytes, entry.SHA256); objectErr == nil {
				if err := db.WithContext(ctx).Model(&persistence.Artifact{}).
					Where("id = ?", entry.ArtifactID).
					Updates(map[string]any{"bucket": destination.Bucket(), "object_version": previous.ObjectVersion}).Error; err != nil {
					return report, fmt.Errorf("restore migrated artifact metadata %s: %w", entry.ArtifactID, err)
				}
				report.Replayed++
				continue
			}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return report, fmt.Errorf("inspect artifact migration %s: %w", entry.ArtifactID, err)
		}

		reader, err := source.Open(ctx, entry.SourceObjectKey)
		if err != nil {
			return report, fmt.Errorf("open source artifact %s: %w", entry.ArtifactID, err)
		}
		info, putErr := destination.Put(ctx, entry.SourceObjectKey, reader, entry.SizeBytes, entry.ContentType)
		closeErr := reader.Close()
		if putErr != nil {
			return report, fmt.Errorf("upload artifact %s: %w", entry.ArtifactID, putErr)
		}
		if closeErr != nil {
			return report, fmt.Errorf("close source artifact %s: %w", entry.ArtifactID, closeErr)
		}
		if err := validateObject(ctx, destination, entry.SourceObjectKey, entry.SizeBytes, entry.SHA256); err != nil {
			return report, fmt.Errorf("verify migrated artifact %s: %w", entry.ArtifactID, err)
		}

		objectVersion := optionalString(info.Version)
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&persistence.Artifact{}).
				Where("id = ? AND status = ? AND sha256 = ? AND size_bytes = ?", entry.ArtifactID, "ready", entry.SHA256, entry.SizeBytes).
				Updates(map[string]any{"bucket": destination.Bucket(), "object_version": objectVersion})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("artifact metadata is missing or changed")
			}
			return tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "artifact_id"}, {Name: "destination"}},
				DoUpdates: clause.Assignments(map[string]any{
					"source_sha256": entry.SHA256, "object_version": objectVersion, "migrated_at": time.Now().UTC(),
				}),
			}).Create(&persistence.ArtifactPayloadMigration{
				ArtifactID: entry.ArtifactID, Destination: destinationID, SourceSHA256: entry.SHA256,
				ObjectVersion: objectVersion, MigratedAt: time.Now().UTC(),
			}).Error
		})
		if err != nil {
			return report, fmt.Errorf("commit artifact migration %s: %w", entry.ArtifactID, err)
		}
		report.Migrated++
	}
	return report, nil
}

func validateObject(ctx context.Context, store artifacts.Store, objectKey string, expectedSize int64, expectedSHA256 string) error {
	info, err := store.Stat(ctx, objectKey)
	if err != nil {
		return err
	}
	if info.Size != expectedSize {
		return fmt.Errorf("size mismatch: expected %d, got %d", expectedSize, info.Size)
	}
	reader, err := store.Open(ctx, objectKey)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := copyWithContext(ctx, hash, io.LimitReader(reader, expectedSize+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != expectedSize || hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			stored, writeErr := destination.Write(buffer[:count])
			written += int64(stored)
			if writeErr != nil {
				return written, writeErr
			}
			if stored != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
