package executiontargets

import (
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

// WorkerManifestHasSupportedStrictProcessContainment is the single stored-
// manifest compatibility predicate. Newly registered manifests are checked at
// ingestion, but every durable consumer must repeat this exact version fence
// so a previously persisted signed v1 manifest cannot regain authority.
func WorkerManifestHasSupportedStrictProcessContainment(manifest persistence.WorkerManifest) bool {
	mode := strings.TrimSpace(manifest.ProcessContainmentMode)
	if manifest.ProcessContainmentSupervisorVersion == nil ||
		manifest.ProcessContainmentProbeVersion == nil || *manifest.ProcessContainmentProbeVersion <= 0 ||
		manifest.ProcessContainmentProbeSHA256 == nil ||
		!validStoredProcessContainmentSHA256(strings.TrimSpace(*manifest.ProcessContainmentProbeSHA256)) ||
		manifest.ProcessContainmentSupervisorIdentity == nil ||
		manifest.ProcessContainmentProviderIdentity == nil {
		return false
	}
	supervisorVersion := strings.TrimSpace(*manifest.ProcessContainmentSupervisorVersion)
	supervisorIdentity := strings.TrimSpace(*manifest.ProcessContainmentSupervisorIdentity)
	providerIdentity := strings.TrimSpace(*manifest.ProcessContainmentProviderIdentity)
	if supervisorIdentity == "" || providerIdentity == "" || supervisorIdentity == providerIdentity {
		return false
	}
	switch mode {
	case "cgroup-v2":
		return manifest.OperatingSystem == "linux" &&
			supervisorVersion == ProtectedCgroupSupervisorVersionV2
	case "job-object":
		return manifest.OperatingSystem == "windows" && supervisorVersion != ""
	default:
		return false
	}
}

func WorkerManifestHasSupportedStrictCgroupV2Containment(manifest persistence.WorkerManifest) bool {
	return strings.TrimSpace(manifest.ProcessContainmentMode) == "cgroup-v2" &&
		WorkerManifestHasSupportedStrictProcessContainment(manifest)
}

func WorkerManifestReportsUnsupportedCgroupSupervisor(manifest persistence.WorkerManifest) bool {
	return strings.TrimSpace(manifest.ProcessContainmentMode) == "cgroup-v2" &&
		(manifest.ProcessContainmentSupervisorVersion == nil ||
			strings.TrimSpace(*manifest.ProcessContainmentSupervisorVersion) != ProtectedCgroupSupervisorVersionV2)
}

func validStoredProcessContainmentSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
