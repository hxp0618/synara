//go:build !linux

package agentd

import (
	"context"
	"errors"
	"io"
)

func RunProtectedCgroupCommand(context.Context, []string, io.Writer) (bool, error) {
	return false, nil
}

func buildProtectedCgroupPreflightReport(context.Context, Config) (*ProtectedCgroupPreflightReport, error) {
	return &ProtectedCgroupPreflightReport{}, nil
}

func protectedCgroupProbeSHA256() string { return "" }

func protectedCgroupIdentityString(identity ProtectedCgroupIdentity) string {
	return ""
}

var (
	protectedCgroupProbeHook = buildProtectedCgroupPreflightReport
	protectedCgroupKeyLoader = func(ProtectedCgroupAttestationConfig) ([]byte, []byte, error) {
		return nil, nil, errors.New("protected cgroup attestation is unsupported on this operating system")
	}
)
