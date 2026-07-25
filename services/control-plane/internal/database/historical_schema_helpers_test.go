package database

import (
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"gorm.io/gorm"
)

// insertPreContainmentWorkerManifest writes only the columns present before the
// Stage 4 process-containment migrations. Historical migration tests must not
// let newly-added persistence fields leak into an older schema fixture.
func insertPreContainmentWorkerManifest(db *gorm.DB, manifest *persistence.WorkerManifest) error {
	return db.Select(
		"id", "manifest_hash", "worker_build_version", "worker_build_git_sha",
		"worker_protocol_minimum", "worker_protocol_maximum",
		"runtime_event_minimum", "runtime_event_maximum",
		"operating_system", "architecture", "image_digest", "feature_flags", "created_at",
	).Create(manifest).Error
}
