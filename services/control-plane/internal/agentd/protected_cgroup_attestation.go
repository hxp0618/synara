package agentd

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/containmentattestation"
)

const (
	protectedCgroupContainmentMode    = "cgroup-v2"
	protectedCgroupSupervisorVersion  = "agentd-protected-cgroup-supervisor-v2"
	protectedCgroupProbeVersion       = 1
	protectedCgroupProbeHelperCommand = "protected-cgroup-probe-helper"
	protectedCgroupProbeChildCommand  = "protected-cgroup-probe-child"
	protectedCgroupPreflightCommand   = "protected-cgroup-preflight"
)

type ProtectedCgroupAttestationConfig struct {
	KeyID          string
	PrivateKeyPath string
}

type ProtectedCgroupPreflightReport struct {
	Enabled                    bool                             `json:"enabled"`
	Mode                       string                           `json:"mode,omitempty"`
	SupervisorVersion          string                           `json:"supervisorVersion,omitempty"`
	ProbeVersion               int                              `json:"probeVersion,omitempty"`
	ProbeSHA256                string                           `json:"probeSha256,omitempty"`
	SupervisorIdentity         string                           `json:"supervisorIdentity,omitempty"`
	ProviderIdentity           string                           `json:"providerIdentity,omitempty"`
	UseCgroupFD                bool                             `json:"useCgroupFD"`
	SetsidDescendantKilled     bool                             `json:"setsidDescendantKilled"`
	ProviderUID                uint32                           `json:"providerUid,omitempty"`
	ProviderGID                uint32                           `json:"providerGid,omitempty"`
	AttestationKeyID           string                           `json:"attestationKeyId,omitempty"`
	AttestationPublicKeySHA256 string                           `json:"attestationPublicKeySha256,omitempty"`
	AttestationPublicKeyBase64 string                           `json:"attestationPublicKeyBase64,omitempty"`
	Attestation                *containmentattestation.Envelope `json:"attestation,omitempty"`
}

func (r *ProtectedCgroupPreflightReport) workerRuntimeProcessContainmentCapability() map[string]any {
	if r == nil || !r.Enabled || r.Attestation == nil {
		return nil
	}
	result := map[string]any{
		"mode":               r.Mode,
		"supervisorVersion":  r.SupervisorVersion,
		"probeVersion":       r.ProbeVersion,
		"probeSha256":        r.ProbeSHA256,
		"supervisorIdentity": r.SupervisorIdentity,
		"providerIdentity":   r.ProviderIdentity,
		"attestation":        *r.Attestation,
	}
	return result
}

func copyProcessContainmentCapability(capability map[string]any) map[string]any {
	if len(capability) == 0 {
		return nil
	}
	result := make(map[string]any, len(capability))
	for key, value := range capability {
		result[key] = value
	}
	return result
}

func encodeProtectedCgroupPublicKeyBase64(publicKey []byte) string {
	if len(publicKey) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(publicKey)
}

func protectedCgroupPreflightContext(
	ctx context.Context,
	cfg Config,
) (context.Context, context.CancelFunc) {
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}
