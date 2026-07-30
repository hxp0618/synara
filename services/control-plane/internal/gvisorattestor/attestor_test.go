package gvisorattestor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObserveRuntimeRequiresRunscExecutableAndContainerdHandler(t *testing.T) {
	runtimeDigest := strings.Repeat("a", 64)
	observation, err := ObserveRuntime(
		[]byte("runsc version release-20260729.0"),
		[]byte("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\nruntime_type = \"io.containerd.runsc.v1\""),
		runtimeDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Version == "" || observation.BinaryDigest != runtimeDigest || len(observation.ConfigDigest) != 64 {
		t.Fatalf("runtime observation = %#v", observation)
	}
	if observation.Version != "runsc version release-20260729.0" {
		t.Fatalf("normalized runtime version = %q", observation.Version)
	}
	if _, err := ObserveRuntime([]byte("runc version 1.4"), []byte("runsc"), runtimeDigest); err == nil {
		t.Fatal("runc executable was accepted as runsc")
	}
	if _, err := ObserveRuntime([]byte("runsc version release"), []byte("[plugins.runtimes.runc]\nruntime_type = \"io.containerd.runc.v2\""), runtimeDigest); err == nil {
		t.Fatal("containerd configuration without runsc was accepted")
	}
	if _, err := ObserveRuntime(
		[]byte("runsc version release"),
		[]byte("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\nruntime_type = \"io.containerd.notrunsc.v1\""),
		runtimeDigest,
	); err == nil {
		t.Fatal("containerd configuration with a lookalike runsc shim was accepted")
	}
	if _, err := ObserveRuntime(
		[]byte("runsc version release"),
		[]byte("[plugins.\"unapproved\".containerd.runtimes.runsc]\nruntime_type = \"io.containerd.runsc.v1\""),
		runtimeDigest,
	); err == nil {
		t.Fatal("containerd configuration under an unapproved plugin section was accepted")
	}
	if _, err := ObserveRuntime(
		[]byte("runsc version release"),
		[]byte("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\nruntime_type = \"io.containerd.runsc.v1\" trailing"),
		runtimeDigest,
	); err == nil {
		t.Fatal("containerd configuration with a malformed runtime type was accepted")
	}
	if _, err := ObserveRuntime(
		[]byte("runsc version release"),
		[]byte("[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]\nruntime_type = 'io.containerd.runsc.v1' # approved"),
		runtimeDigest,
	); err != nil {
		t.Fatalf("containerd v2 runsc handler was rejected: %v", err)
	}
	if strings.ToLower(observation.ConfigDigest) != observation.ConfigDigest {
		t.Fatalf("runtime configuration digest is not normalized: %q", observation.ConfigDigest)
	}
	if _, err := ObserveRuntime(
		[]byte("runsc version release"),
		[]byte("[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\nruntime_type = \"io.containerd.runsc.v1\""),
		strings.Repeat("A", 64),
	); err == nil {
		t.Fatal("non-canonical runtime binary digest was accepted")
	}
}

func TestHashBoundedFileBindsTheExactRuntimeBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(path, []byte("runsc-binary-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := hashBoundedFile(path, 64)
	if err != nil || !lowerHexSHA256(first) {
		t.Fatalf("runtime binary digest = %q, %v", first, err)
	}
	if err := os.WriteFile(path, []byte("runsc-binary-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := hashBoundedFile(path, 64)
	if err != nil || first == second {
		t.Fatalf("runtime binary replacement did not change digest: %q, %q, %v", first, second, err)
	}
	if _, err := hashBoundedFile(path, 4); err == nil {
		t.Fatal("oversized runtime binary was hashed")
	}
}

func TestBoundedWriterRejectsUnboundedRuntimeOutput(t *testing.T) {
	writer := &boundedWriter{maximum: 4}
	if written, err := writer.Write([]byte("runsc-version")); err != nil || written != len("runsc-version") {
		t.Fatalf("bounded write = %d, %v", written, err)
	}
	if !writer.overflow || string(writer.data) != "runs" {
		t.Fatalf("bounded writer = %#v", writer)
	}
}
