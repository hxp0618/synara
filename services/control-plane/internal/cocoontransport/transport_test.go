package cocoontransport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuestTransportBridgesProviderProtocolAndAttestedProfile(t *testing.T) {
	providerHost := providerHostTestHelper(t)
	toGuestReader, toGuestWriter := io.Pipe()
	fromGuestReader, fromGuestWriter := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runGuest(
			ctx,
			[]string{"--", providerHost, "-test.run=^TestCocoonTransportProviderHelper$", "--", "anonymous"},
			toGuestReader,
			fromGuestWriter,
			io.Discard,
		)
	}()
	wire := &frameWriter{writer: toGuestWriter}
	encoded, err := json.Marshal(bootstrap{Credential: json.RawMessage("null"), Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.write(frameBootstrap, encoded); err != nil {
		t.Fatal(err)
	}
	if err := wire.write(frameProviderStdin, []byte("describe\n")); err != nil {
		t.Fatal(err)
	}
	if err := wire.write(frameProviderStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		message, err := readFrame(fromGuestReader)
		if err != nil {
			t.Fatal(err)
		}
		switch message.kind {
		case frameProviderStdout:
			output.Write(message.payload)
		case frameProviderExit:
			if uint32FromBytes(message.payload) != 0 {
				t.Fatalf("Provider helper exit = %d", uint32FromBytes(message.payload))
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != "profile=microvm-isolated-v1 input=describe\n" {
				t.Fatalf("guest Provider output = %q", got)
			}
			return
		}
	}
}

func TestGuestTransportRelaysBrokeredCredentialTrafficWithoutUpstreamSecret(t *testing.T) {
	providerHost := providerHostTestHelper(t)
	toGuestReader, toGuestWriter := io.Pipe()
	fromGuestReader, fromGuestWriter := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runGuest(
			ctx,
			[]string{"--", providerHost, "-test.run=^TestCocoonTransportProviderHelper$", "--", "credential"},
			toGuestReader,
			fromGuestWriter,
			io.Discard,
		)
	}()
	credential := json.RawMessage(`{"payload":{"apiKey":"task-token","baseUrl":"` + credentialBrokerPlaceholder + `"}}`)
	encoded, err := json.Marshal(bootstrap{Credential: credential, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	wire := &frameWriter{writer: toGuestWriter}
	if err := wire.write(frameBootstrap, encoded); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var request bytes.Buffer
	var tunnelID uint64
	for {
		message, err := readFrame(fromGuestReader)
		if err != nil {
			t.Fatal(err)
		}
		switch message.kind {
		case frameTunnelOpen:
			tunnelID, _, err = decodeTunnelPayload(message.payload)
			if err != nil {
				t.Fatal(err)
			}
		case frameTunnelGuestData:
			id, payload, err := decodeTunnelPayload(message.payload)
			if err != nil || id != tunnelID {
				t.Fatalf("unexpected tunnel data id=%d err=%v", id, err)
			}
			request.Write(payload)
			if strings.Contains(request.String(), "\r\n\r\n") {
				if !strings.Contains(request.String(), "Authorization: Bearer task-token") ||
					!strings.Contains(request.String(), "GET /v1/models") {
					t.Fatalf("broker tunnel request = %q", request.String())
				}
				response := "HTTP/1.1 200 OK\r\nContent-Length: 9\r\nConnection: close\r\n\r\nbroker-ok"
				if err := wire.write(frameTunnelHostData, tunnelPayload(tunnelID, []byte(response))); err != nil {
					t.Fatal(err)
				}
				if err := wire.write(frameTunnelHostClose, tunnelPayload(tunnelID, nil)); err != nil {
					t.Fatal(err)
				}
			}
		case frameProviderStdout:
			output.Write(message.payload)
		case frameProviderExit:
			if uint32FromBytes(message.payload) != 0 {
				t.Fatalf("credential Provider helper exit = %d", uint32FromBytes(message.payload))
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != "broker-ok\n" {
				t.Fatalf("brokered guest Provider output = %q", got)
			}
			return
		}
	}
}

func TestHostBootstrapRejectsNonLoopbackCredentialBroker(t *testing.T) {
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readPipe.Close(); _ = writePipe.Close() })
	credential := `{"payload":{"apiKey":"task-token","baseUrl":"http://10.0.0.2:8080"}}` + "\n"
	if _, err := io.WriteString(writePipe, credential); err != nil {
		t.Fatal(err)
	}
	_ = writePipe.Close()
	t.Setenv(credentialFDEnvironment, fmt.Sprint(readPipe.Fd()))
	if _, _, err := hostBootstrapFromEnvironment(); err == nil || !strings.Contains(err.Error(), "endpoint is invalid") {
		t.Fatalf("non-loopback credential broker returned %v", err)
	}
}

func TestProviderInputCloseAfterGuestExitIsBenign(t *testing.T) {
	for _, err := range []error{nil, io.EOF, io.ErrClosedPipe, os.ErrClosed} {
		if !benignProviderInputShutdown(err) {
			t.Fatalf("Provider input shutdown %v was treated as a transport failure", err)
		}
	}
	if benignProviderInputShutdown(fmt.Errorf("read failed")) {
		t.Fatal("an unrelated Provider input failure was suppressed")
	}
}

func TestCocoonTransportProviderHelper(t *testing.T) {
	if len(os.Args) < 2 || (os.Args[len(os.Args)-1] != "anonymous" && os.Args[len(os.Args)-1] != "credential") {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "anonymous" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Printf("profile=%s input=%s", os.Getenv(outerSandboxProfileEnvironment), input)
		os.Exit(0)
	}
	fd, err := strconvAtoiForTest(os.Getenv(credentialFDEnvironment))
	if err != nil {
		os.Exit(3)
	}
	file := os.NewFile(uintptr(fd), "credential")
	contents, err := io.ReadAll(file)
	if err != nil {
		os.Exit(4)
	}
	var credential struct {
		Payload struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseUrl"`
		} `json:"payload"`
	}
	if json.Unmarshal(contents, &credential) != nil || credential.Payload.APIKey != "task-token" ||
		!strings.HasPrefix(credential.Payload.BaseURL, "http://127.0.0.1:") {
		os.Exit(5)
	}
	request, _ := http.NewRequest(http.MethodGet, credential.Payload.BaseURL+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential.Payload.APIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		os.Exit(6)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	_, _ = fmt.Println(string(body))
	os.Exit(0)
}

func providerHostTestHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider-host")
	if err := os.Symlink(os.Args[0], path); err != nil {
		t.Fatal(err)
	}
	return path
}

func strconvAtoiForTest(value string) (int, error) {
	var result int
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid")
		}
		result = result*10 + int(digit-'0')
	}
	return result, nil
}
