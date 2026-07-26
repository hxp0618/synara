package executions

import (
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestProjectWorkerProcessContainmentReclassifiesPersistedSignedV1AsUntrusted(t *testing.T) {
	supervisorVersion := "agentd-protected-cgroup-supervisor-v1"
	probeVersion := 1
	probeSHA256 := strings.Repeat("a", 64)
	supervisorIdentity, providerIdentity := "uid:0 gid:0", "uid:10001 gid:10002"
	keyID, keySHA := "trusted-key", strings.Repeat("b", 64)
	view, err := projectWorkerProcessContainment(persistence.WorkerManifest{
		OperatingSystem: "linux", ProcessContainmentMode: "cgroup-v2",
		ProcessContainmentSupervisorVersion:    &supervisorVersion,
		ProcessContainmentProbeVersion:         &probeVersion,
		ProcessContainmentProbeSHA256:          &probeSHA256,
		ProcessContainmentSupervisorIdentity:   &supervisorIdentity,
		ProcessContainmentProviderIdentity:     &providerIdentity,
		ProcessContainmentTrustMode:            executiontargets.ProcessContainmentTrustSignedV1,
		ProcessContainmentAttestationKeyID:     &keyID,
		ProcessContainmentAttestationKeySHA256: &keySHA,
	}, persistence.ExecutionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if view.TrustState != "untrusted" || view.ReasonCode == nil ||
		*view.ReasonCode != "unsupported-supervisor-version" {
		t.Fatalf("persisted v1 projection = %#v", view)
	}
}
