package agentd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func TestKubernetesNetworkBoundaryVerifierWaitsForConsecutiveBlockedRounds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dials := 0
	err := verifyKubernetesNetworkBoundary(ctx, kubernetesNetworkBoundaryProbe{
		endpoints:         []string{"metadata-a:80", "metadata-b:80"},
		consecutivePasses: 2,
		retryInterval:     time.Millisecond,
		dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			dials++
			if dials == 1 {
				client, server := net.Pipe()
				_ = server.Close()
				return client, nil
			}
			return nil, errors.New("blocked")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dials != 5 {
		t.Fatalf("metadata dials = %d, want 5", dials)
	}
}

func TestKubernetesNetworkBoundaryVerifierFailsClosedWhileMetadataIsReachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := verifyKubernetesNetworkBoundary(ctx, kubernetesNetworkBoundaryProbe{
		endpoints: []string{"metadata:80"}, consecutivePasses: 1,
		retryInterval: time.Millisecond,
		dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	})
	if err == nil {
		t.Fatal("reachable metadata endpoint passed the network boundary")
	}
}

func TestKubernetesNetworkBoundaryVerifierArgumentDispatch(t *testing.T) {
	handled, err := RunKubernetesNetworkBoundaryVerifier([]string{"agentd"})
	if handled || err != nil {
		t.Fatalf("unrelated arguments handled=%t err=%v", handled, err)
	}
	if platform.KubernetesNetworkBoundaryVerifyArgument != "--verify-kubernetes-network-boundary" {
		t.Fatalf("network boundary argument = %q", platform.KubernetesNetworkBoundaryVerifyArgument)
	}
}
