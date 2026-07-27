package agentd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const providerHostPrestartMaximumAttempts = 3

type providerHostV2Prestart struct {
	process     *providerHostV2Process
	descriptors map[string]providerHostDescriptor
	released    chan struct{}
	releaseOnce sync.Once
}

func (p *providerHostV2Prestart) release() {
	p.releaseOnce.Do(func() { close(p.released) })
}

type providerHostV2PrestartManager struct {
	runner         *Runner
	ctx            context.Context
	cancel         context.CancelFunc
	requestTimeout time.Duration

	mu        sync.Mutex
	host      *providerHostV2Prestart
	finished  bool
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
}

type providerHostV2Adoption struct {
	process             *providerHostV2Process
	runtimeEventVersion int
}

func providerHostPrestartEnabled(config Config) bool {
	return effectiveWorkerMode(config) == executions.WorkerModeWarmPool &&
		strings.TrimSpace(config.CgroupV2Root) == "" &&
		config.RunnerProtocol == RunnerProtocolV2
}

func (r *Runner) providerPrestartExecutionID() uuid.UUID {
	namespace := r.instanceUID
	if namespace == uuid.Nil {
		namespace = uuid.NameSpaceOID
	}
	return uuid.NewSHA1(namespace, []byte("synara-provider-prestart"))
}

func (r *Runner) startProviderHostV2Prestart(ctx context.Context, requestTimeout time.Duration) {
	prestartContext, cancel := context.WithCancel(ctx)
	manager := &providerHostV2PrestartManager{
		runner: r, ctx: prestartContext, cancel: cancel, requestTimeout: requestTimeout,
		ready: make(chan struct{}), done: make(chan struct{}),
	}
	r.prestartMu.Lock()
	if r.providerHostPrestart != nil {
		r.prestartMu.Unlock()
		cancel()
		return
	}
	r.providerHostPrestart = manager
	r.prestartMu.Unlock()
	go manager.run()
	select {
	case <-manager.ready:
	case <-manager.done:
	case <-ctx.Done():
	}
}

func (r *Runner) stopProviderHostV2Prestart() {
	r.prestartMu.Lock()
	manager := r.providerHostPrestart
	r.providerHostPrestart = nil
	r.prestartMu.Unlock()
	if manager != nil {
		manager.stop()
	}
}

func (r *Runner) takeProviderHostV2Prestart(
	input RunnerInput,
	credential *RunnerCredential,
) (providerHostV2Adoption, bool, string) {
	r.prestartMu.Lock()
	manager := r.providerHostPrestart
	r.providerHostPrestart = nil
	r.prestartMu.Unlock()
	if manager == nil {
		return providerHostV2Adoption{}, false, ""
	}
	adoption, reason := manager.take(input, credential)
	return adoption, true, reason
}

func (m *providerHostV2PrestartManager) run() {
	defer func() {
		m.mu.Lock()
		m.finished = true
		m.mu.Unlock()
		m.readyOnce.Do(func() { close(m.ready) })
		close(m.done)
	}()
	for attempt := 0; attempt < providerHostPrestartMaximumAttempts; attempt++ {
		if attempt > 0 && !waitContext(m.ctx, time.Duration(attempt)*100*time.Millisecond) {
			return
		}
		attemptContext := m.ctx
		cancelAttempt := func() {}
		if m.requestTimeout > 0 {
			attemptContext, cancelAttempt = context.WithTimeout(m.ctx, m.requestTimeout)
		}
		host, err := m.runner.prestartProviderHostV2(attemptContext)
		cancelAttempt()
		if err != nil {
			continue
		}
		m.mu.Lock()
		if m.ctx.Err() != nil {
			m.mu.Unlock()
			_ = host.process.abort()
			return
		}
		m.host = host
		m.mu.Unlock()
		m.readyOnce.Do(func() { close(m.ready) })

		select {
		case <-m.ctx.Done():
			if m.remove(host) {
				_ = host.process.abort()
			}
			return
		case <-host.released:
			return
		case <-host.process.readerDone:
			if !m.remove(host) {
				return
			}
			_ = host.process.abort()
		}
	}
}

func (m *providerHostV2PrestartManager) remove(host *providerHostV2Prestart) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.host != host {
		return false
	}
	m.host = nil
	return true
}

func (m *providerHostV2PrestartManager) take(
	input RunnerInput,
	credential *RunnerCredential,
) (providerHostV2Adoption, string) {
	m.mu.Lock()
	host := m.host
	if host != nil {
		m.host = nil
		host.release()
	}
	finished := m.finished
	m.mu.Unlock()
	if host == nil {
		m.stop()
		if finished {
			return providerHostV2Adoption{}, "retries-exhausted"
		}
		return providerHostV2Adoption{}, "not-ready"
	}
	m.cancel()
	<-m.done
	if len(input.ProviderEnvironment) > 0 {
		_ = host.process.abort()
		return providerHostV2Adoption{}, "execution-environment-required"
	}
	if !host.process.availableForAdoption() {
		_ = host.process.abort()
		return providerHostV2Adoption{}, "process-exited"
	}
	descriptor, found := host.descriptors[normalizeProvider(input.Workload.Provider)]
	if !found {
		_ = host.process.abort()
		return providerHostV2Adoption{}, "provider-not-described"
	}
	runtimeEventVersion, err := m.runner.configureProviderHostV2ForExecution(
		host.process, descriptor, input, credential,
	)
	if err != nil {
		_ = host.process.abort()
		return providerHostV2Adoption{}, "descriptor-mismatch"
	}
	if err := host.process.deliverCredential(credential); err != nil {
		_ = host.process.abort()
		return providerHostV2Adoption{}, "credential-delivery"
	}
	return providerHostV2Adoption{
		process: host.process, runtimeEventVersion: runtimeEventVersion,
	}, "adopted"
}

func (m *providerHostV2PrestartManager) stop() {
	m.cancel()
	<-m.done
}

func (r *Runner) prestartProviderHostV2(ctx context.Context) (host *providerHostV2Prestart, err error) {
	process, err := r.startProviderHostV2DeferredCredential(ctx, r.providerPrestartExecutionID(), 1)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			err = errors.Join(err, process.abort())
		}
	}()
	descriptors := make(map[string]providerHostDescriptor, len(providerHostProviders))
	executionID := r.providerPrestartExecutionID().String()
	for _, provider := range providerHostProviders {
		command := newProviderHostCommand(
			executionID, 1, "Describe", "worker-prestart:"+provider,
			map[string]any{"provider": provider},
		)
		terminal, executeErr := process.executeContext(ctx, command, nil)
		if executeErr != nil {
			return nil, executeErr
		}
		descriptor, descriptorErr := descriptorFromResult(terminal)
		if descriptorErr != nil {
			return nil, descriptorErr
		}
		if validateErr := r.validateProviderHostDescriptorWire(descriptor, provider); validateErr != nil {
			return nil, validateErr
		}
		descriptors[normalizeProvider(provider)] = descriptor
	}
	owned = false
	return &providerHostV2Prestart{
		process: process, descriptors: descriptors, released: make(chan struct{}),
	}, nil
}
