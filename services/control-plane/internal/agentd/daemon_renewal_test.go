package agentd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestRenewLeaseLoopRetriesAfterBoundedTransportStall(t *testing.T) {
	var requestCount atomic.Int32
	secondRequest := make(chan struct{})
	var secondRequestOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requestCount.Add(1) == 1 {
			time.Sleep(200 * time.Millisecond)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		secondRequestOnce.Do(func() { close(secondRequest) })
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		ControlPlaneURL:    baseURL,
		RequestTimeout:     time.Second,
		LeaseRenewInterval: 20 * time.Millisecond,
	}
	daemon := &Daemon{
		config: config,
		client: NewClient(config),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	daemon.client.workerToken = "worker-token"
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	var fatalCancelCalled atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.renewLeaseLoop(
			ctx,
			uuid.New(),
			executions.Lease{TenantID: uuid.New(), Generation: 1, LeaseToken: "lease-token"},
			func() { fatalCancelCalled.Store(true) },
			result,
		)
	}()

	select {
	case <-secondRequest:
	case <-time.After(500 * time.Millisecond):
		cancel()
		<-done
		t.Fatalf("renewal did not retry after the first request stalled; requests = %d", requestCount.Load())
	}
	cancel()
	<-done
	if fatalCancelCalled.Load() {
		t.Fatal("retryable renewal stall cancelled the Execution")
	}
	for renewErr := range result {
		if renewErr != nil {
			t.Fatalf("retryable renewal stall surfaced as fatal: %v", renewErr)
		}
	}
}

func TestExecutionLeaseRenewalErrorIgnoresCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := executionLeaseRenewalError(ctx, context.Canceled); err != nil {
		t.Fatalf("caller cancellation surfaced as a renewal failure: %v", err)
	}
}

func TestExecutionLeaseRenewalErrorRetriesTransportFailure(t *testing.T) {
	cause := errors.New("renew transport failed")
	if err := executionLeaseRenewalError(context.Background(), cause); err != nil {
		t.Fatalf("transport failure surfaced as fatal: %v", err)
	}
}

func TestExecutionLeaseRenewalErrorRetriesTransientHTTPFailure(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cause := &controlPlaneProblem{Status: status, Code: "renew_unavailable"}
			if err := executionLeaseRenewalError(context.Background(), cause); err != nil {
				t.Fatalf("HTTP %d renewal failure surfaced as fatal: %v", status, err)
			}
		})
	}
}

func TestExecutionLeaseRenewalErrorPreservesFencingFailure(t *testing.T) {
	cause := &controlPlaneProblem{Status: http.StatusConflict, Code: "lease_expired"}
	err := executionLeaseRenewalError(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("renewal error = %v, want wrapped fencing failure", err)
	}
}
