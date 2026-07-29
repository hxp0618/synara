package cocoonsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	HealthProtocolVersion = 1
	HostSupervisorVersion = "v1"
	ProviderTransport     = "vsock-v2"
	IsolationProfile      = "microvm-isolated-v1"

	maximumHealthMessageBytes = 4096
	healthConnectionTimeout   = 2 * time.Second
	maximumHealthConnections  = 16
)

type Readiness struct {
	KVMReady                bool
	VSockListenerReady      bool
	CredentialBrokerReady   bool
	GuestIdentityFenceReady bool
	WorkspaceMountReady     bool
}

func (r Readiness) Ready() bool {
	return r.KVMReady && r.VSockListenerReady && r.CredentialBrokerReady &&
		r.GuestIdentityFenceReady && r.WorkspaceMountReady
}

// ReadinessObserver returns a cached, non-blocking supervisor state snapshot.
// The health server may call it concurrently and never treats the probe itself
// as evidence that a transport or credential boundary is operational.
type ReadinessObserver func(context.Context) Readiness

type healthRequest struct {
	Operation string `json:"operation"`
	Version   int    `json:"version"`
}

type HealthResponse struct {
	Ready             bool   `json:"ready"`
	HostSupervisor    string `json:"hostSupervisor"`
	ProviderTransport string `json:"providerTransport"`
	IsolationProfile  string `json:"isolationProfile"`
}

func responseFor(readiness Readiness) HealthResponse {
	return HealthResponse{
		Ready: readiness.Ready(), HostSupervisor: HostSupervisorVersion,
		ProviderTransport: ProviderTransport, IsolationProfile: IsolationProfile,
	}
}

func ServeHealthSocket(ctx context.Context, socketPath string, observe ReadinessObserver) error {
	if observe == nil {
		return errors.New("Cocoon supervisor readiness observer is required")
	}
	socketPath = filepath.Clean(strings.TrimSpace(socketPath))
	if !filepath.IsAbs(socketPath) || socketPath == string(filepath.Separator) {
		return errors.New("Cocoon supervisor health socket path must be an absolute file path")
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return errors.New("Cocoon supervisor health socket path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Cocoon supervisor health socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on Cocoon supervisor health socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return fmt.Errorf("protect Cocoon supervisor health socket: %w", err)
	}
	created, err := os.Lstat(socketPath)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return fmt.Errorf("inspect created Cocoon supervisor health socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		if current, statErr := os.Lstat(socketPath); statErr == nil && os.SameFile(created, current) {
			_ = os.Remove(socketPath)
		}
	}()

	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-serveContext.Done()
		_ = listener.Close()
	}()
	semaphore := make(chan struct{}, maximumHealthConnections)
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept Cocoon supervisor health connection: %w", acceptErr)
		}
		select {
		case semaphore <- struct{}{}:
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer func() { <-semaphore }()
				serveHealthConnection(serveContext, connection, observe)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func serveHealthConnection(ctx context.Context, connection net.Conn, observe ReadinessObserver) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(healthConnectionTimeout))
	request, err := readHealthRequest(connection)
	readiness := Readiness{}
	if err == nil && request.Operation == "attest" && request.Version == HealthProtocolVersion {
		readiness = observe(ctx)
	}
	_ = json.NewEncoder(connection).Encode(responseFor(readiness))
}

func readHealthRequest(reader io.Reader) (healthRequest, error) {
	buffered := bufio.NewReaderSize(reader, maximumHealthMessageBytes+1)
	line, err := buffered.ReadString('\n')
	if err != nil {
		return healthRequest{}, err
	}
	if len(line) > maximumHealthMessageBytes {
		return healthRequest{}, errors.New("Cocoon supervisor health request is oversized")
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	var request healthRequest
	if err := decoder.Decode(&request); err != nil {
		return healthRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return healthRequest{}, errors.New("Cocoon supervisor health request contains trailing JSON")
	}
	return request, nil
}
