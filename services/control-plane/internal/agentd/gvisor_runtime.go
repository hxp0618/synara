package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

func RunGVisorRuntimeVerifier(args []string) (bool, error) {
	if len(args) != 2 || args[1] != platform.GVisorRuntimeVerifyArgument {
		return false, nil
	}
	version, err := os.ReadFile("/proc/version")
	if err != nil {
		return true, fmt.Errorf("read runtime kernel identity: %w", err)
	}
	return true, verifyGVisorRuntime(version, "/tmp")
}

func verifyGVisorRuntime(version []byte, temporaryRoot string) error {
	if !bytes.Contains(bytes.ToLower(version), []byte("gvisor")) {
		return errors.New("runtime kernel identity is not gVisor")
	}
	if !filepath.IsAbs(temporaryRoot) {
		return errors.New("runtime verifier temporary root must be absolute")
	}
	file, err := os.CreateTemp(temporaryRoot, ".synara-gvisor-canary-")
	if err != nil {
		return fmt.Errorf("create runtime verifier file: %w", err)
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	payload := []byte("synara-gvisor-runtime-v1")
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write runtime verifier file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync runtime verifier file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runtime verifier file: %w", err)
	}
	observed, err := os.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read runtime verifier file: %w", err)
	}
	if !bytes.Equal(observed, payload) {
		return errors.New("runtime verifier file contents changed")
	}
	if err := exec.Command("/bin/sh", "-c", "exit 0").Run(); err != nil {
		return fmt.Errorf("run runtime verifier child process: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on runtime verifier loopback: %w", err)
	}
	defer listener.Close()
	serverResult := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		request := make([]byte, len(payload))
		if _, readErr := io.ReadFull(connection, request); readErr != nil {
			serverResult <- readErr
			return
		}
		if !bytes.Equal(request, payload) {
			serverResult <- errors.New("runtime verifier loopback payload changed")
			return
		}
		_, writeErr := connection.Write(payload)
		serverResult <- writeErr
	}()
	connection, err := net.DialTimeout("tcp4", listener.Addr().String(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial runtime verifier loopback: %w", err)
	}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		_ = connection.Close()
		return fmt.Errorf("write runtime verifier loopback: %w", err)
	}
	response := make([]byte, len(payload))
	_, readErr := io.ReadFull(connection, response)
	closeErr := connection.Close()
	if readErr != nil {
		return fmt.Errorf("read runtime verifier loopback: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close runtime verifier loopback: %w", closeErr)
	}
	if serverErr := <-serverResult; serverErr != nil {
		return fmt.Errorf("serve runtime verifier loopback: %w", serverErr)
	}
	if !bytes.Equal(response, payload) {
		return errors.New("runtime verifier loopback response changed")
	}
	return nil
}
