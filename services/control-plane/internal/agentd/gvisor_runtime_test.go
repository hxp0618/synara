package agentd

import (
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestRunGVisorRuntimeVerifierDispatch(t *testing.T) {
	handled, err := RunGVisorRuntimeVerifier([]string{"synara-agentd", "unrelated"})
	if handled || err != nil {
		t.Fatalf("unrelated gVisor verifier dispatch = handled %t, err %v", handled, err)
	}
	if platform.GVisorRuntimeVerifyArgument != "--verify-gvisor-runtime" {
		t.Fatalf("gVisor verifier argument = %q", platform.GVisorRuntimeVerifyArgument)
	}
}

func TestVerifyGVisorRuntimeRequiresIdentityAndPrivateFilesystem(t *testing.T) {
	if err := verifyGVisorRuntime([]byte("Linux version 6.8.0 host"), t.TempDir()); err == nil {
		t.Fatal("native Linux identity was accepted as gVisor")
	}
	if err := verifyGVisorRuntime([]byte("Linux version 4.4.0 gVisor"), "relative"); err == nil {
		t.Fatal("relative temporary root was accepted")
	}
	if err := verifyGVisorRuntime([]byte("Linux version 4.4.0 gVisor"), t.TempDir()); err != nil {
		t.Fatalf("verify gVisor runtime: %v", err)
	}
}
