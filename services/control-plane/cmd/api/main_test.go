package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestControlPlaneHTTPShutdownCancelsActiveRequestAfterClosingListener(t *testing.T) {
	runtimeContext, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	requestStarted := make(chan struct{})
	requestStopped := make(chan struct{})
	server := newControlPlaneHTTPServer(
		"127.0.0.1:0",
		http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(requestStarted)
			<-request.Context().Done()
			close(requestStopped)
		}),
		runtimeContext,
		stopRuntime,
	)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	clientDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		clientDone <- requestErr
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not reach the server")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStopped:
	default:
		t.Fatal("active request context was not cancelled during shutdown")
	}
	select {
	case <-runtimeContext.Done():
	default:
		t.Fatal("runtime context was not cancelled during shutdown")
	}
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP client did not finish after shutdown")
	}
}
