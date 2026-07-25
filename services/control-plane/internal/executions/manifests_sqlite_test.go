package executions

import (
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestSQLiteWorkerManifestProcessContainmentEvidenceIsValidatedAndImmutable(t *testing.T) {
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "sqlite-containment-manifest")
	cleanupWorkers(t, db, worker.ID)
	if worker.CurrentManifestID == nil {
		t.Fatal("Worker registration did not attach a Manifest")
	}
	var manifest persistence.WorkerManifest
	if err := db.Where("id = ?", *worker.CurrentManifestID).Take(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	if !workerManifestSupportsStrictResourceSuspendContainment(manifest) {
		t.Fatalf("SQLite lost strict process-containment evidence: %#v", manifest)
	}
	if manifest.ProcessContainmentTrustMode != "signed-v1" || manifest.ProcessContainmentAttestationKeyID == nil ||
		manifest.ProcessContainmentAttestationKeySHA256 == nil {
		t.Fatalf("SQLite lost verified process-containment provenance: %#v", manifest)
	}
	if err := db.Model(&persistence.WorkerManifest{}).
		Where("id = ?", manifest.ID).
		Update("process_containment_provider_identity", manifest.ProcessContainmentSupervisorIdentity).Error; err == nil {
		t.Fatal("SQLite allowed immutable process-containment identities to be rewritten")
	}
	if err := db.Model(&persistence.WorkerManifest{}).
		Where("id = ?", manifest.ID).
		Update("process_containment_trust_mode", "legacy-untrusted").Error; err == nil {
		t.Fatal("SQLite allowed immutable process-containment trust provenance to be rewritten")
	}
}
