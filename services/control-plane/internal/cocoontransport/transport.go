package cocoontransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	credentialFDEnvironment        = "SYNARA_PROVIDER_CREDENTIAL_FD"
	outerSandboxProfileEnvironment = "SYNARA_PROVIDER_OUTER_SANDBOX_PROFILE"
	microVMIsolationProfile        = "microvm-isolated-v1"
	credentialBrokerPlaceholder    = "http://synara-cocoon-vsock.invalid"
	maximumCredentialBytes         = 65 << 10
)

var guestEnvironmentAllowlist = []string{
	"SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS",
	"SYNARA_PROVIDER_NPM_CONFIG_USERCONFIG",
	"SYNARA_PROVIDER_PIP_CONFIG_FILE",
}

type bootstrap struct {
	Credential  json.RawMessage   `json:"credential"`
	Environment map[string]string `json:"environment"`
}

type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("Cocoon guest Provider Host exited with code %d", e.Code)
}

func Main(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("Cocoon Provider transport mode is required")
	}
	switch arguments[0] {
	case "host":
		return runHost(ctx, arguments[1:], stdin, stdout, stderr)
	case "guest":
		return runGuest(ctx, arguments[1:], stdin, stdout, stderr)
	default:
		return errors.New("Cocoon Provider transport mode is unsupported")
	}
}

type hostOptions struct {
	vmID            string
	cocoonCommand   string
	guestCommand    string
	providerCommand []string
}

func parseHostOptions(arguments []string) (hostOptions, error) {
	options := hostOptions{
		cocoonCommand: "/usr/local/bin/cocoon",
		guestCommand:  "/usr/local/bin/synara-cocoon-provider-transport",
	}
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(arguments) {
		return hostOptions{}, errors.New("Cocoon Provider transport requires a guest Provider command")
	}
	for index := 0; index < separator; index++ {
		switch arguments[index] {
		case "--vm-id", "--cocoon-command", "--guest-command":
			if index+1 >= separator {
				return hostOptions{}, errors.New("Cocoon Provider transport option is missing a value")
			}
			value := strings.TrimSpace(arguments[index+1])
			if value == "" || strings.ContainsAny(value, "\r\n\x00") {
				return hostOptions{}, errors.New("Cocoon Provider transport option is invalid")
			}
			switch arguments[index] {
			case "--vm-id":
				options.vmID = value
			case "--cocoon-command":
				options.cocoonCommand = value
			case "--guest-command":
				options.guestCommand = value
			}
			index++
		default:
			return hostOptions{}, fmt.Errorf("unsupported Cocoon Provider transport option %q", arguments[index])
		}
	}
	if options.vmID == "" || len(options.vmID) > 128 || strings.ContainsAny(options.vmID, "/\\\r\n\t\x00") {
		return hostOptions{}, errors.New("Cocoon Provider transport VM identity is invalid")
	}
	options.providerCommand = append([]string(nil), arguments[separator+1:]...)
	for _, value := range options.providerCommand {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return hostOptions{}, errors.New("Cocoon guest Provider command is invalid")
		}
	}
	if filepath.Base(options.providerCommand[0]) != "provider-host" {
		return hostOptions{}, errors.New("Cocoon guest command must start the Synara Provider Host")
	}
	return options, nil
}

func runHost(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseHostOptions(arguments)
	if err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv(outerSandboxProfileEnvironment)) != microVMIsolationProfile {
		return errors.New("Cocoon Provider transport requires a supervisor-attested microVM profile")
	}
	initial, brokerAddress, err := hostBootstrapFromEnvironment()
	if err != nil {
		return err
	}
	encodedBootstrap, err := json.Marshal(initial)
	if err != nil {
		return fmt.Errorf("encode Cocoon Provider bootstrap: %w", err)
	}
	childArguments := []string{"vm", "exec", "-i", options.vmID, "--", options.guestCommand, "guest", "--"}
	childArguments = append(childArguments, options.providerCommand...)
	command := exec.CommandContext(ctx, options.cocoonCommand, childArguments...)
	childStdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Cocoon guest transport input: %w", err)
	}
	childStdout, err := command.StdoutPipe()
	if err != nil {
		_ = childStdin.Close()
		return fmt.Errorf("open Cocoon guest transport output: %w", err)
	}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = childStdin.Close()
		return fmt.Errorf("start Cocoon guest transport: %w", err)
	}
	wire := &frameWriter{writer: childStdin}
	if err := wire.write(frameBootstrap, encodedBootstrap); err != nil {
		_ = childStdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("send Cocoon Provider bootstrap: %w", err)
	}
	inputDone := make(chan error, 1)
	go func() {
		inputDone <- copyStreamToFrames(stdin, wire, frameProviderStdin, frameProviderStdinEOF)
	}()
	tunnels := newTunnelSet()
	defer tunnels.closeAll()
	providerExit := -1
	for {
		message, readErr := readFrame(childStdout)
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				_ = command.Process.Kill()
			}
			break
		}
		switch message.kind {
		case frameProviderStdout:
			if _, err := stdout.Write(message.payload); err != nil {
				_ = command.Process.Kill()
				return err
			}
		case frameProviderStderr:
			if _, err := stderr.Write(message.payload); err != nil {
				_ = command.Process.Kill()
				return err
			}
		case frameProviderExit:
			if len(message.payload) != 4 {
				_ = command.Process.Kill()
				return errors.New("Cocoon Provider exit frame is invalid")
			}
			providerExit = int(uint32FromBytes(message.payload))
		case frameTunnelOpen:
			id, _, err := decodeTunnelPayload(message.payload)
			if err != nil || brokerAddress == "" {
				if err == nil {
					_ = wire.write(frameTunnelHostClose, tunnelPayload(id, nil))
				}
				continue
			}
			connection, dialErr := net.DialTimeout("tcp", brokerAddress, 2*time.Second)
			if dialErr != nil || !tunnels.add(id, connection) {
				if connection != nil {
					_ = connection.Close()
				}
				_ = wire.write(frameTunnelHostClose, tunnelPayload(id, nil))
				continue
			}
			go pumpTunnel(connection, id, tunnels, wire, frameTunnelHostData, frameTunnelHostClose)
		case frameTunnelGuestData:
			id, payload, err := decodeTunnelPayload(message.payload)
			if err != nil {
				return err
			}
			if connection := tunnels.get(id); connection != nil {
				if _, err := connection.Write(payload); err != nil {
					tunnels.close(id)
					_ = wire.write(frameTunnelHostClose, tunnelPayload(id, nil))
				}
			}
		case frameTunnelGuestClose:
			id, _, err := decodeTunnelPayload(message.payload)
			if err != nil {
				return err
			}
			tunnels.close(id)
		default:
			return errors.New("Cocoon guest sent an unsupported transport frame")
		}
	}
	_ = childStdin.Close()
	waitErr := command.Wait()
	select {
	case inputErr := <-inputDone:
		if !benignProviderInputShutdown(inputErr) {
			return inputErr
		}
	default:
	}
	if providerExit >= 0 {
		if providerExit != 0 {
			return ExitError{Code: providerExit}
		}
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("Cocoon guest transport stopped: %w", waitErr)
	}
	return errors.New("Cocoon guest transport stopped without a Provider exit status")
}

func benignProviderInputShutdown(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}

func hostBootstrapFromEnvironment() (bootstrap, string, error) {
	result := bootstrap{Credential: json.RawMessage("null"), Environment: map[string]string{}}
	for _, name := range guestEnvironmentAllowlist {
		if value := os.Getenv(name); value != "" {
			if strings.ContainsAny(value, "\r\n\x00") {
				return bootstrap{}, "", errors.New("Cocoon Provider environment contains control characters")
			}
			result.Environment[name] = value
		}
	}
	rawFD := strings.TrimSpace(os.Getenv(credentialFDEnvironment))
	if rawFD == "" {
		return result, "", nil
	}
	fd, err := strconv.Atoi(rawFD)
	if err != nil || fd < 3 || fd > 1024 {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor is invalid")
	}
	file := os.NewFile(uintptr(fd), "provider-credential")
	if file == nil {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor is unavailable")
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maximumCredentialBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor could not be consumed")
	}
	if len(contents) > maximumCredentialBytes {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor is oversized")
	}
	var credential map[string]any
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(&credential); err != nil || credential == nil {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor contains trailing data")
	}
	payload, ok := credential["payload"].(map[string]any)
	if !ok {
		return bootstrap{}, "", errors.New("Cocoon Provider credential payload is invalid")
	}
	baseURL, ok := payload["baseUrl"].(string)
	brokerURL, err := url.Parse(strings.TrimSpace(baseURL))
	if !ok || err != nil || brokerURL.Scheme != "http" || brokerURL.User != nil || brokerURL.Path != "" ||
		brokerURL.RawQuery != "" || brokerURL.Fragment != "" || brokerURL.Port() == "" || !loopbackHostname(brokerURL.Hostname()) {
		return bootstrap{}, "", errors.New("Cocoon Provider credential broker endpoint is invalid")
	}
	payload["baseUrl"] = credentialBrokerPlaceholder
	encoded, err := json.Marshal(credential)
	if err != nil {
		return bootstrap{}, "", errors.New("Cocoon Provider credential descriptor could not be encoded")
	}
	result.Credential = encoded
	return result, brokerURL.Host, nil
}

func loopbackHostname(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func parseGuestCommand(arguments []string) ([]string, error) {
	if len(arguments) < 2 || arguments[0] != "--" {
		return nil, errors.New("Cocoon guest transport requires a Provider command")
	}
	command := append([]string(nil), arguments[1:]...)
	if filepath.Base(command[0]) != "provider-host" {
		return nil, errors.New("Cocoon guest transport may only start the Synara Provider Host")
	}
	for _, value := range command {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("Cocoon guest Provider command is invalid")
		}
	}
	return command, nil
}

func runGuest(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	providerCommand, err := parseGuestCommand(arguments)
	if err != nil {
		return err
	}
	first, err := readFrame(stdin)
	if err != nil || first.kind != frameBootstrap {
		return errors.New("Cocoon guest Provider bootstrap is missing")
	}
	var initial bootstrap
	decoder := json.NewDecoder(bytes.NewReader(first.payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&initial); err != nil {
		return errors.New("Cocoon guest Provider bootstrap is invalid")
	}
	wire := &frameWriter{writer: stdout}
	tunnels := newTunnelSet()
	defer tunnels.closeAll()
	credential := initial.Credential
	if len(credential) == 0 {
		credential = json.RawMessage("null")
	}
	var listener net.Listener
	if string(credential) != "null" {
		listener, credential, err = prepareGuestCredential(credential)
		if err != nil {
			return err
		}
		defer listener.Close()
		go acceptGuestTunnels(ctx, listener, tunnels, wire)
	}
	command := exec.CommandContext(ctx, providerCommand[0], providerCommand[1:]...)
	command.Env = guestProviderEnvironment(initial.Environment)
	providerStdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	providerStdout, providerStdoutWriter := io.Pipe()
	providerStderr, providerStderrWriter := io.Pipe()
	command.Stdout = providerStdoutWriter
	command.Stderr = providerStderrWriter
	var credentialWriter *os.File
	if string(credential) != "null" {
		credentialReader, writer, err := os.Pipe()
		if err != nil {
			_ = providerStdoutWriter.Close()
			_ = providerStderrWriter.Close()
			return err
		}
		command.ExtraFiles = []*os.File{credentialReader}
		command.Env = append(command.Env, credentialFDEnvironment+"=3")
		credentialWriter = writer
		defer credentialReader.Close()
	}
	if err := command.Start(); err != nil {
		_ = providerStdoutWriter.Close()
		_ = providerStderrWriter.Close()
		if credentialWriter != nil {
			_ = credentialWriter.Close()
		}
		return fmt.Errorf("start guest Provider Host: %w", err)
	}
	if credentialWriter != nil {
		if _, err := credentialWriter.Write(append(credential, '\n')); err != nil {
			_ = credentialWriter.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = providerStdoutWriter.Close()
			_ = providerStderrWriter.Close()
			return errors.New("deliver guest Provider credential descriptor")
		}
		_ = credentialWriter.Close()
	}
	outputDone := make(chan error, 2)
	go func() { outputDone <- copyStreamToFrames(providerStdout, wire, frameProviderStdout, 0) }()
	go func() { outputDone <- copyStreamToFrames(providerStderr, wire, frameProviderStderr, 0) }()
	processDone := make(chan error, 1)
	go func() {
		waitErr := command.Wait()
		_ = providerStdoutWriter.Close()
		_ = providerStderrWriter.Close()
		processDone <- waitErr
	}()
	frames := make(chan frame)
	frameErrors := make(chan error, 1)
	go func() {
		defer close(frames)
		for {
			message, err := readFrame(stdin)
			if err != nil {
				frameErrors <- err
				return
			}
			frames <- message
		}
	}()
	for {
		select {
		case message, ok := <-frames:
			if !ok {
				_ = providerStdin.Close()
				_ = command.Process.Kill()
				<-processDone
				return <-frameErrors
			}
			switch message.kind {
			case frameProviderStdin:
				if _, err := providerStdin.Write(message.payload); err != nil {
					_ = command.Process.Kill()
					return err
				}
			case frameProviderStdinEOF:
				_ = providerStdin.Close()
			case frameTunnelHostData:
				id, payload, err := decodeTunnelPayload(message.payload)
				if err != nil {
					return err
				}
				if connection := tunnels.get(id); connection != nil {
					if _, err := connection.Write(payload); err != nil {
						tunnels.close(id)
						_ = wire.write(frameTunnelGuestClose, tunnelPayload(id, nil))
					}
				}
			case frameTunnelHostClose:
				id, _, err := decodeTunnelPayload(message.payload)
				if err != nil {
					return err
				}
				tunnels.close(id)
			default:
				return errors.New("Cocoon host sent an unsupported transport frame")
			}
		case waitErr := <-processDone:
			_ = providerStdin.Close()
			for range 2 {
				if outputErr := <-outputDone; outputErr != nil {
					return outputErr
				}
			}
			exitCode := 0
			if waitErr != nil {
				var exitError *exec.ExitError
				if errors.As(waitErr, &exitError) {
					exitCode = exitError.ExitCode()
				} else {
					exitCode = 1
				}
			}
			payload := make([]byte, 4)
			putUint32(payload, uint32(exitCode))
			if err := wire.write(frameProviderExit, payload); err != nil {
				return err
			}
			return nil
		}
	}
}

func prepareGuestCredential(raw json.RawMessage) (net.Listener, json.RawMessage, error) {
	var credential map[string]any
	if err := json.Unmarshal(raw, &credential); err != nil || credential == nil {
		return nil, nil, errors.New("Cocoon guest Provider credential descriptor is invalid")
	}
	payload, ok := credential["payload"].(map[string]any)
	if !ok || payload["baseUrl"] != credentialBrokerPlaceholder {
		return nil, nil, errors.New("Cocoon guest Provider credential broker binding is invalid")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen for Cocoon guest credential tunnel: %w", err)
	}
	payload["baseUrl"] = "http://" + listener.Addr().String()
	encoded, err := json.Marshal(credential)
	if err != nil {
		_ = listener.Close()
		return nil, nil, errors.New("encode Cocoon guest Provider credential descriptor")
	}
	return listener, encoded, nil
}

func guestProviderEnvironment(source map[string]string) []string {
	values := map[string]string{
		"PATH": "/opt/synara/provider-tools/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin",
		"HOME": "/root", "TMPDIR": "/tmp", "LANG": "C.UTF-8",
		outerSandboxProfileEnvironment: microVMIsolationProfile,
	}
	for _, name := range guestEnvironmentAllowlist {
		if value := source[name]; value != "" && !strings.ContainsAny(value, "\r\n\x00") {
			values[name] = value
		}
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func acceptGuestTunnels(ctx context.Context, listener net.Listener, tunnels *tunnelSet, wire *frameWriter) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var nextID atomic.Uint64
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		id := nextID.Add(1)
		if !tunnels.add(id, connection) || wire.write(frameTunnelOpen, tunnelPayload(id, nil)) != nil {
			_ = connection.Close()
			continue
		}
		go pumpTunnel(connection, id, tunnels, wire, frameTunnelGuestData, frameTunnelGuestClose)
	}
}

type tunnelSet struct {
	mu          sync.Mutex
	connections map[uint64]net.Conn
}

func newTunnelSet() *tunnelSet {
	return &tunnelSet{connections: make(map[uint64]net.Conn)}
}

func (s *tunnelSet) add(id uint64, connection net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == 0 || connection == nil || s.connections[id] != nil {
		return false
	}
	s.connections[id] = connection
	return true
}

func (s *tunnelSet) get(id uint64) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections[id]
}

func (s *tunnelSet) close(id uint64) {
	s.mu.Lock()
	connection := s.connections[id]
	delete(s.connections, id)
	s.mu.Unlock()
	if connection != nil {
		_ = connection.Close()
	}
}

func (s *tunnelSet) closeAll() {
	s.mu.Lock()
	connections := s.connections
	s.connections = make(map[uint64]net.Conn)
	s.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func pumpTunnel(
	connection net.Conn,
	id uint64,
	tunnels *tunnelSet,
	wire *frameWriter,
	dataKind, closeKind byte,
) {
	buffer := make([]byte, streamChunkBytes)
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			if writeErr := wire.write(dataKind, tunnelPayload(id, buffer[:count])); writeErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	tunnels.close(id)
	_ = wire.write(closeKind, tunnelPayload(id, nil))
}

func copyStreamToFrames(reader io.Reader, wire *frameWriter, dataKind, eofKind byte) error {
	buffer := make([]byte, streamChunkBytes)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if writeErr := wire.write(dataKind, buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if eofKind != 0 {
					return wire.write(eofKind, nil)
				}
				return nil
			}
			return err
		}
	}
}

func uint32FromBytes(value []byte) uint32 {
	return uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
}

func putUint32(destination []byte, value uint32) {
	destination[0] = byte(value >> 24)
	destination[1] = byte(value >> 16)
	destination[2] = byte(value >> 8)
	destination[3] = byte(value)
}
