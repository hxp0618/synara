package agentd

import (
	"context"
	"encoding/json"
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
	releaseFirstRequest := make(chan struct{})
	var secondRequestOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requestCount.Add(1) == 1 {
			<-releaseFirstRequest
			return
		}
		secondRequestOnce.Do(func() { close(secondRequest) })
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseFirstRequest) })
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
			nil,
			nil,
		)
	}()

	select {
	case <-secondRequest:
	case <-time.After(300 * time.Millisecond):
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

func TestClientRenewDecodesLeaseResponse(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	workerID := uuid.New()
	heartbeatAt := time.Now().UTC().Add(30 * time.Second).Truncate(time.Millisecond)
	expiresAt := heartbeatAt.Add(time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/v1/workers/executions/"+executionID.String()+"/renew" {
			t.Fatalf("unexpected renew request: %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(executions.Lease{
			ExecutionID: executionID,
			TenantID:    tenantID,
			WorkerID:    workerID,
			Generation:  9,
			LeaseToken:  "renewed-lease-token",
			HeartbeatAt: heartbeatAt,
			ExpiresAt:   expiresAt,
		})
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second})
	client.workerToken = "worker-token"

	renewed, err := client.Renew(context.Background(), executionID, executions.Lease{
		TenantID: tenantID, Generation: 9, LeaseToken: "lease-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.ExecutionID != executionID || renewed.LeaseToken != "renewed-lease-token" ||
		!renewed.HeartbeatAt.Equal(heartbeatAt) || !renewed.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected renewed lease: %#v", renewed)
	}
}

func TestRenewLeaseLoopPublishesLatestLeaseUpdatesNonBlocking(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		count := requestCount.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(executions.Lease{
			ExecutionID: executionID,
			TenantID:    tenantID,
			Generation:  1,
			LeaseToken:  "lease-token",
			HeartbeatAt: time.Now().UTC(),
			ExpiresAt:   time.Now().UTC().Add(time.Minute),
			AcquiredAt:  time.Unix(int64(count), 0).UTC(),
		})
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		config: Config{
			ControlPlaneURL:    baseURL,
			RequestTimeout:     100 * time.Millisecond,
			LeaseRenewInterval: 20 * time.Millisecond,
		},
		client: NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: 100 * time.Millisecond}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	daemon.client.workerToken = "worker-token"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	updates := make(chan executions.Lease, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.renewLeaseLoop(
			ctx,
			executionID,
			executions.Lease{TenantID: tenantID, Generation: 1, LeaseToken: "lease-token"},
			func() {},
			result,
			updates,
			nil,
		)
	}()

	time.Sleep(90 * time.Millisecond)
	cancel()
	<-done
	var latest executions.Lease
	for update := range updates {
		latest = update
	}
	if requestCount.Load() < 2 {
		t.Fatalf("renew loop did not publish multiple renewals; requests=%d", requestCount.Load())
	}
	if latest.ExecutionID != executionID || latest.Generation != 1 {
		t.Fatalf("unexpected latest lease update: %#v", latest)
	}
}

func TestRenewLeaseLoopDoesNotPublishRenewalStartedBeforeCredentialAccessIsReady(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	grantID := uuid.New()
	firstRequest := make(chan struct{})
	var firstRequestOnce sync.Once
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			firstRequestOnce.Do(func() { close(firstRequest) })
		}
		lease := executions.Lease{
			ExecutionID: executionID, TenantID: tenantID, Generation: 1,
			HeartbeatAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute),
		}
		if count > 1 {
			lease.ProviderCredentialAccess = &executions.ProviderCredentialAccess{
				GrantID: grantID, Serial: int64(count), Status: "active",
			}
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(lease)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		config: Config{
			ControlPlaneURL: baseURL, RequestTimeout: 100 * time.Millisecond,
			LeaseRenewInterval: 15 * time.Millisecond,
		},
		client: NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: 100 * time.Millisecond}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	daemon.client.workerToken = "worker-token"
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	updates := make(chan executions.Lease, 1)
	var accessReady atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.renewLeaseLoop(
			ctx,
			executionID,
			executions.Lease{TenantID: tenantID, Generation: 1, LeaseToken: "lease-token"},
			func() {},
			result,
			updates,
			&accessReady,
		)
	}()

	select {
	case <-firstRequest:
		accessReady.Store(true)
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("first pre-access renewal did not start")
	}
	select {
	case renewed := <-updates:
		if renewed.ProviderCredentialAccess == nil || renewed.ProviderCredentialAccess.GrantID != grantID {
			cancel()
			<-done
			t.Fatalf("published stale pre-access renewal: %#v", renewed)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("post-access renewal was not published")
	}
	cancel()
	<-done
}

func TestRenewLeaseLoopPublishesInFlightRenewalThatReturnsAccessAfterReady(t *testing.T) {
	executionID := uuid.New()
	tenantID := uuid.New()
	grantID := uuid.New()
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var requestStartedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requestStartedOnce.Do(func() { close(requestStarted) })
		<-releaseResponse
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(executions.Lease{
			ExecutionID: executionID, TenantID: tenantID, Generation: 1,
			HeartbeatAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute),
			ProviderCredentialAccess: &executions.ProviderCredentialAccess{
				GrantID: grantID, Serial: 1, Status: "active",
			},
		})
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		config: Config{
			ControlPlaneURL: baseURL, RequestTimeout: time.Second,
			LeaseRenewInterval: 50 * time.Millisecond,
		},
		client: NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	daemon.client.workerToken = "worker-token"
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	updates := make(chan executions.Lease, 1)
	var accessReady atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.renewLeaseLoop(
			ctx,
			executionID,
			executions.Lease{TenantID: tenantID, Generation: 1, LeaseToken: "lease-token"},
			func() {},
			result,
			updates,
			&accessReady,
		)
	}()

	select {
	case <-requestStarted:
		accessReady.Store(true)
		close(releaseResponse)
	case <-time.After(time.Second):
		cancel()
		close(releaseResponse)
		<-done
		t.Fatal("in-flight renewal did not start")
	}
	select {
	case renewed := <-updates:
		if renewed.ProviderCredentialAccess == nil || renewed.ProviderCredentialAccess.GrantID != grantID {
			cancel()
			<-done
			t.Fatalf("in-flight access projection was not published: %#v", renewed)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("in-flight renewal access projection was dropped after readiness")
	}
	cancel()
	<-done
}
