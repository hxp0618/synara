package agentd

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const (
	kubernetesNetworkBoundaryTimeout           = 60 * time.Second
	kubernetesNetworkBoundaryDialTimeout       = 750 * time.Millisecond
	kubernetesNetworkBoundaryRetryInterval     = 250 * time.Millisecond
	kubernetesNetworkBoundaryConsecutivePasses = 2
)

var kubernetesNetworkBoundaryEndpoints = []string{
	"169.254.169.254:80",
	"100.100.100.200:80",
	"[fd00:ec2::254]:80",
}

type kubernetesNetworkBoundaryDial func(context.Context, string, string) (net.Conn, error)

type kubernetesNetworkBoundaryProbe struct {
	endpoints         []string
	consecutivePasses int
	retryInterval     time.Duration
	dial              kubernetesNetworkBoundaryDial
}

// RunKubernetesNetworkBoundaryVerifier handles the dedicated init-container
// mode that keeps agentd and Provider processes stopped until every supported
// cloud metadata endpoint is unreachable for consecutive probe rounds.
func RunKubernetesNetworkBoundaryVerifier(args []string) (bool, error) {
	if len(args) != 2 || args[1] != platform.KubernetesNetworkBoundaryVerifyArgument {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesNetworkBoundaryTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: kubernetesNetworkBoundaryDialTimeout}
	err := verifyKubernetesNetworkBoundary(ctx, kubernetesNetworkBoundaryProbe{
		endpoints:         append([]string(nil), kubernetesNetworkBoundaryEndpoints...),
		consecutivePasses: kubernetesNetworkBoundaryConsecutivePasses,
		retryInterval:     kubernetesNetworkBoundaryRetryInterval,
		dial:              dialer.DialContext,
	})
	return true, err
}

func verifyKubernetesNetworkBoundary(ctx context.Context, probe kubernetesNetworkBoundaryProbe) error {
	if len(probe.endpoints) == 0 || probe.consecutivePasses < 1 || probe.retryInterval < 0 || probe.dial == nil {
		return errors.New("invalid Kubernetes network boundary probe")
	}
	consecutive := 0
	for {
		blocked := true
		for _, endpoint := range probe.endpoints {
			connection, err := probe.dial(ctx, "tcp", endpoint)
			if err != nil {
				continue
			}
			_ = connection.Close()
			blocked = false
			break
		}
		if blocked {
			consecutive++
			if consecutive >= probe.consecutivePasses {
				return nil
			}
		} else {
			consecutive = 0
		}
		timer := time.NewTimer(probe.retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.New("Kubernetes network boundary did not become effective before the deadline")
		case <-timer.C:
		}
	}
}
