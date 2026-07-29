package cocoonsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Daemon struct {
	config     Config
	logger     *slog.Logger
	instanceID uuid.UUID
	now        func() time.Time

	readinessMu sync.RWMutex
	readiness   Readiness
	workers     map[uuid.UUID]*workerHandle
	retryAfter  map[uuid.UUID]time.Time
	completed   map[uuid.UUID]Pod
}

type workerHandle struct {
	pod    Pod
	cancel context.CancelFunc
	done   <-chan error
}

func NewDaemon(config Config, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	if len(config.GuestProviderCommand) == 0 {
		config.GuestProviderCommand = []string{"/usr/local/bin/provider-host"}
	}
	return &Daemon{
		config: config, logger: logger, instanceID: uuid.New(), now: time.Now,
		workers: make(map[uuid.UUID]*workerHandle), retryAfter: make(map[uuid.UUID]time.Time),
		completed: make(map[uuid.UUID]Pod),
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.prepareRoots(); err != nil {
		return err
	}
	healthErr := make(chan error, 1)
	go func() {
		healthErr <- ServeHealthSocket(ctx, d.config.HealthSocketPath, func(context.Context) Readiness {
			return d.currentReadiness()
		})
	}()
	defer d.stopAllWorkers()
	defer func() {
		clearContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.publishNodeAttestation(clearContext, false); err != nil {
			d.logger.Warn("clear Cocoon supervisor attestation", "error", err)
		}
	}()

	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()
	if err := d.reconcile(ctx); err != nil {
		d.logger.Warn("initial Cocoon supervisor reconciliation failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-healthErr:
			if err == nil && ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("serve Cocoon supervisor health: %w", err)
		case <-ticker.C:
			if err := d.reconcile(ctx); err != nil {
				d.logger.Warn("Cocoon supervisor reconciliation failed", "error", err)
			}
		}
	}
}

func (d *Daemon) prepareRoots() error {
	for _, path := range []string{
		filepath.Dir(d.config.HealthSocketPath), d.config.StateRoot,
		filepath.Join(d.config.StateRoot, "workers"), d.runtimeWorkersRoot(),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare Cocoon supervisor directory %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect Cocoon supervisor directory %s: %w", path, err)
		}
	}
	return nil
}

func (d *Daemon) runtimeWorkersRoot() string {
	return filepath.Join(filepath.Dir(d.config.HealthSocketPath), "workers")
}

func (d *Daemon) reconcile(ctx context.Context) error {
	d.reapCompletedWorkers()
	readiness := d.observeStaticReadiness()
	pods, listErr := d.listAssignedPods(ctx)
	readiness.GuestIdentityFenceReady = listErr == nil
	d.setReadiness(readiness)
	if publishErr := d.publishNodeAttestation(ctx, readiness.Ready()); publishErr != nil {
		readiness.GuestIdentityFenceReady = false
		d.setReadiness(readiness)
		return publishErr
	}
	if listErr != nil {
		return listErr
	}
	desired := make(map[uuid.UUID]Pod, len(pods))
	for _, pod := range pods {
		desired[pod.UID] = pod
	}
	d.pruneCompletedAssignments(desired)
	for uid, worker := range d.workers {
		pod, found := desired[uid]
		if !found || pod.VMID != worker.pod.VMID || pod.ExecutionID != worker.pod.ExecutionID || pod.Generation != worker.pod.Generation {
			worker.cancel()
		}
	}
	for uid, pod := range desired {
		if _, found := d.workers[uid]; found || d.assignmentCompleted(pod) || d.now().Before(d.retryAfter[uid]) {
			continue
		}
		workerContext, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		d.workers[uid] = &workerHandle{pod: pod, cancel: cancel, done: done}
		go func() {
			done <- d.runWorker(workerContext, pod)
			close(done)
		}()
		d.logger.Info("materializing Cocoon host agentd", "pod", pod.Name, "podUid", pod.UID, "vmId", pod.VMID, "executionId", pod.ExecutionID, "generation", pod.Generation)
	}
	return nil
}

func samePodAssignment(left, right Pod) bool {
	return left.UID == right.UID && left.VMID == right.VMID && left.ExecutionID == right.ExecutionID &&
		left.Generation == right.Generation && left.GuestImage == right.GuestImage
}

func (d *Daemon) assignmentCompleted(pod Pod) bool {
	completed, found := d.completed[pod.UID]
	return found && samePodAssignment(completed, pod)
}

func (d *Daemon) pruneCompletedAssignments(desired map[uuid.UUID]Pod) {
	for uid, completed := range d.completed {
		pod, found := desired[uid]
		if !found || !samePodAssignment(completed, pod) {
			delete(d.completed, uid)
		}
	}
}

func (d *Daemon) observeStaticReadiness() Readiness {
	regularExecutable := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
	}
	_, kvmErr := os.Stat("/dev/kvm")
	_, vsockErr := os.Stat("/dev/vhost-vsock")
	return Readiness{
		KVMReady: kvmErr == nil,
		VSockListenerReady: vsockErr == nil && regularExecutable(d.config.CocoonCommand) &&
			regularExecutable(d.config.TransportCommand),
		CredentialBrokerReady: regularExecutable(d.config.AgentdCommand) && regularExecutable(d.config.TransportCommand),
		WorkspaceMountReady:   regularExecutable(d.config.VirtiofsdCommand),
	}
}

func (d *Daemon) setReadiness(readiness Readiness) {
	d.readinessMu.Lock()
	d.readiness = readiness
	d.readinessMu.Unlock()
}

func (d *Daemon) currentReadiness() Readiness {
	d.readinessMu.RLock()
	defer d.readinessMu.RUnlock()
	return d.readiness
}

func (d *Daemon) reapCompletedWorkers() {
	for uid, worker := range d.workers {
		select {
		case err := <-worker.done:
			delete(d.workers, uid)
			worker.cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				d.logger.Warn("Cocoon host agentd stopped", "pod", worker.pod.Name, "podUid", uid, "error", err)
				d.retryAfter[uid] = d.now().Add(5 * time.Second)
				delete(d.completed, uid)
			} else if err == nil {
				delete(d.retryAfter, uid)
				d.completed[uid] = worker.pod
				d.logger.Info("Cocoon host agentd completed", "pod", worker.pod.Name, "podUid", uid, "executionId", worker.pod.ExecutionID, "generation", worker.pod.Generation)
			} else {
				delete(d.retryAfter, uid)
				delete(d.completed, uid)
			}
		default:
		}
	}
}

func (d *Daemon) stopAllWorkers() {
	for _, worker := range d.workers {
		worker.cancel()
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for len(d.workers) > 0 {
		for uid, worker := range d.workers {
			select {
			case <-worker.done:
				delete(d.workers, uid)
			default:
			}
		}
		if len(d.workers) == 0 {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}
