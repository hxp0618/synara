//go:build linux

package agentd

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProtectedCgroupIdentityStringIncludesUIDAndGID(t *testing.T) {
	if actual := protectedCgroupIdentityString(ProtectedCgroupIdentity{UID: 10001, GID: 10002}); actual != "uid:10001 gid:10002" {
		t.Fatalf("protectedCgroupIdentityString() = %q", actual)
	}
}

func TestProtectedCgroupProbeHelperReportsIdentityAndEscapedSentinel(t *testing.T) {
	t.Setenv("SYNARA_PROTECTED_CGROUP_SECRET_SENTINEL", "must-not-reach-helper-or-child")
	pipes, err := openProtectedCgroupProbePipes()
	if err != nil {
		t.Fatal(err)
	}
	defer closeProtectedCgroupProbeFiles(
		pipes.reportRead,
		pipes.readyRead,
		pipes.sentinelRead,
		pipes.reportWrite,
		pipes.readyWrite,
		pipes.sentinelWrite,
	)
	command, err := defaultProtectedCgroupProbeCommand(pipes.reportWrite, pipes.readyWrite, pipes.sentinelWrite)
	if err != nil {
		t.Fatal(err)
	}
	if command.Env == nil || len(command.Env) != 0 {
		t.Fatalf("protected cgroup helper environment = %#v, want explicit empty environment", command.Env)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := closeProtectedCgroupProbeFiles(pipes.reportWrite, pipes.readyWrite, pipes.sentinelWrite); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	pipes.reportWrite = nil
	pipes.readyWrite = nil
	pipes.sentinelWrite = nil
	readyContext, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()
	identity, err := waitForProtectedCgroupProbeReady(readyContext, pipes.reportRead, pipes.readyRead)
	if err != nil {
		_ = command.Process.Kill()
		_ = waitProtectedCgroupProbeCommand(command)
		t.Fatal(err)
	}
	if identity.UID != uint32(os.Geteuid()) || identity.GID != uint32(os.Getegid()) {
		_ = command.Process.Kill()
		_ = waitProtectedCgroupProbeCommand(command)
		t.Fatalf("probe helper identity = uid:%d gid:%d, want uid:%d gid:%d", identity.UID, identity.GID, os.Geteuid(), os.Getegid())
	}
	if identity.EnvironmentEntries != 0 {
		_ = command.Process.Kill()
		_ = waitProtectedCgroupProbeCommand(command)
		t.Fatalf("probe helper inherited %d environment entries", identity.EnvironmentEntries)
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	if err := waitProtectedCgroupProbeCommand(command); err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(pipes.sentinelRead)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(payload)) != "escaped" {
		t.Fatalf("probe child sentinel = %q, want escaped", payload)
	}
}
