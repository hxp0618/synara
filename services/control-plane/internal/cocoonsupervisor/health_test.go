package cocoonsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeHealthConnectionRequiresEveryIsolationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		readiness Readiness
		request   string
		wantReady bool
	}{
		{
			name: "complete", request: `{"operation":"attest","version":1}` + "\n", wantReady: true,
			readiness: Readiness{KVMReady: true, VSockListenerReady: true, CredentialBrokerReady: true, GuestIdentityFenceReady: true},
		},
		{
			name: "credential broker missing", request: `{"operation":"attest","version":1}` + "\n",
			readiness: Readiness{KVMReady: true, VSockListenerReady: true, GuestIdentityFenceReady: true},
		},
		{
			name: "unknown operation", request: `{"operation":"health","version":1}` + "\n",
			readiness: Readiness{KVMReady: true, VSockListenerReady: true, CredentialBrokerReady: true, GuestIdentityFenceReady: true},
		},
		{
			name: "unknown field", request: `{"operation":"attest","version":1,"ready":true}` + "\n",
			readiness: Readiness{KVMReady: true, VSockListenerReady: true, CredentialBrokerReady: true, GuestIdentityFenceReady: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			done := make(chan struct{})
			go func() {
				serveHealthConnection(context.Background(), server, func(context.Context) Readiness { return test.readiness })
				close(done)
			}()
			if _, err := client.Write([]byte(test.request)); err != nil {
				t.Fatal(err)
			}
			var response HealthResponse
			if err := json.NewDecoder(bufio.NewReader(client)).Decode(&response); err != nil {
				t.Fatal(err)
			}
			_ = client.Close()
			<-done
			if response.Ready != test.wantReady || response.HostSupervisor != HostSupervisorVersion ||
				response.ProviderTransport != ProviderTransport || response.IsolationProfile != IsolationProfile {
				t.Fatalf("health response = %#v", response)
			}
		})
	}
}

func TestServeHealthSocketProtectsLifecycleAndRefusesExistingPath(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "synara-cocoon-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "health.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHealthSocket(ctx, socketPath, func(context.Context) Readiness {
			return Readiness{KVMReady: true, VSockListenerReady: true, CredentialBrokerReady: true, GuestIdentityFenceReady: true}
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		info, err := os.Lstat(socketPath)
		if err == nil {
			if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
				t.Fatalf("health socket mode = %v", info.Mode())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health socket was not created: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte(`{"operation":"attest","version":1}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var response HealthResponse
	if err := json.NewDecoder(connection).Decode(&response); err != nil || !response.Ready {
		t.Fatalf("live health response = %#v, err=%v", response, err)
	}
	_ = connection.Close()
	if err := ServeHealthSocket(context.Background(), socketPath, func(context.Context) Readiness { return Readiness{} }); err == nil {
		t.Fatal("a second supervisor replaced an existing health socket")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("health socket remained after shutdown: %v", err)
	}
}
