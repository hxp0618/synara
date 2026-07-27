package executions

import (
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestProjectWorkerProcessContainmentReclassifiesPersistedUnsupportedVersionsAsUntrusted(t *testing.T) {
	for _, test := range []struct {
		name              string
		supervisorVersion string
		probeVersion      int
	}{
		{name: "v1 supervisor", supervisorVersion: "agentd-protected-cgroup-supervisor-v1", probeVersion: 1},
		{name: "v2 supervisor", supervisorVersion: "agentd-protected-cgroup-supervisor-v2", probeVersion: 1},
		{name: "v3 old probe", supervisorVersion: executiontargets.ProtectedCgroupSupervisorVersionV3, probeVersion: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			probeSHA256 := strings.Repeat("a", 64)
			supervisorIdentity, providerIdentity := "uid:0 gid:0", "uid:10001 gid:10002"
			keyID, keySHA := "trusted-key", strings.Repeat("b", 64)
			view, err := projectWorkerProcessContainment(persistence.WorkerManifest{
				OperatingSystem: "linux", ProcessContainmentMode: "cgroup-v2",
				ProcessContainmentSupervisorVersion:    &test.supervisorVersion,
				ProcessContainmentProbeVersion:         &test.probeVersion,
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
				*view.ReasonCode != "unsupported-containment-version" {
				t.Fatalf("persisted unsupported projection = %#v", view)
			}
		})
	}
}
