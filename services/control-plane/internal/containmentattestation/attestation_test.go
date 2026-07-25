package containmentattestation

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestAttestationBindsEveryWorkerAndContainmentField(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	statement := Statement{
		ExecutionTargetID: "target", TargetKind: "kubernetes", InstanceUID: "instance",
		ClusterID: "cluster", Namespace: "namespace", PodName: "pod",
		WorkerBuildVersion: "worker", WorkerBuildGitSHA: "abcdef0", ImageDigest: "sha256:digest",
		OperatingSystem: "linux", Architecture: "amd64", Mode: "cgroup-v2",
		SupervisorVersion: "supervisor", ProbeVersion: 1, ProbeSHA256: "probe",
		SupervisorIdentity: "uid:10001", ProviderIdentity: "uid:10002",
	}
	envelope, err := Sign(privateKey, "target-key", statement)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(publicKey, envelope, statement); err != nil {
		t.Fatal(err)
	}

	tampered := statement
	tampered.InstanceUID = "other-instance"
	if err := Verify(publicKey, envelope, tampered); err == nil {
		t.Fatal("attestation accepted a different physical Worker identity")
	}
	tampered = statement
	tampered.ProbeSHA256 = "other-probe"
	if err := Verify(publicKey, envelope, tampered); err == nil {
		t.Fatal("attestation accepted different containment evidence")
	}
	envelope.KeyID = "other-key"
	if err := Verify(publicKey, envelope, statement); err == nil {
		t.Fatal("attestation accepted a different key ID")
	}
}
