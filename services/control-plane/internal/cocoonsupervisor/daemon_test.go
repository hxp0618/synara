package cocoonsupervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReapCompletedWorkerSuppressesSamePodAssignmentReplay(t *testing.T) {
	d := NewDaemon(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod := supervisorTestPod()
	done := make(chan error, 1)
	done <- nil
	close(done)
	d.workers[pod.UID] = &workerHandle{pod: pod, cancel: func() {}, done: done}

	d.reapCompletedWorkers()
	if _, found := d.workers[pod.UID]; found {
		t.Fatal("cleanly completed Worker remained active")
	}
	if !d.assignmentCompleted(pod) {
		t.Fatal("cleanly completed Pod assignment was not tombstoned")
	}
	d.pruneCompletedAssignments(map[uuid.UUID]Pod{pod.UID: pod})
	if !d.assignmentCompleted(pod) {
		t.Fatal("unchanged running Pod assignment lost its completion tombstone")
	}

	replacement := pod
	replacement.Generation++
	replacement.VMID = "replacement-vm"
	d.pruneCompletedAssignments(map[uuid.UUID]Pod{replacement.UID: replacement})
	if d.assignmentCompleted(replacement) || d.assignmentCompleted(pod) {
		t.Fatal("changed Pod assignment retained a stale completion tombstone")
	}
}

func TestReapFailedWorkerRemainsRetryable(t *testing.T) {
	d := NewDaemon(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return base }
	pod := supervisorTestPod()
	done := make(chan error, 1)
	done <- errors.New("agentd failed")
	close(done)
	d.workers[pod.UID] = &workerHandle{pod: pod, cancel: func() {}, done: done}

	d.reapCompletedWorkers()
	if d.assignmentCompleted(pod) {
		t.Fatal("failed Worker was incorrectly tombstoned as completed")
	}
	if got := d.retryAfter[pod.UID]; !got.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("retry deadline = %s, want %s", got, base.Add(5*time.Second))
	}
}

func TestReapCanceledWorkerDoesNotSuppressReplacement(t *testing.T) {
	d := NewDaemon(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod := supervisorTestPod()
	done := make(chan error, 1)
	done <- context.Canceled
	close(done)
	d.workers[pod.UID] = &workerHandle{pod: pod, cancel: func() {}, done: done}

	d.reapCompletedWorkers()
	if d.assignmentCompleted(pod) {
		t.Fatal("canceled Worker was incorrectly tombstoned as completed")
	}
	if _, found := d.retryAfter[pod.UID]; found {
		t.Fatal("canceled Worker retained a retry delay")
	}
}

func supervisorTestPod() Pod {
	return Pod{
		Name: "synara-worker-1", Namespace: "synara-workers", UID: uuid.New(),
		VMID: "vm-1", ExecutionID: uuid.New(), Generation: 1,
		GuestImage: "registry.example/synara/guest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}
