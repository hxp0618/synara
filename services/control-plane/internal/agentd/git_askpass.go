package agentd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

const GitAskPassSocketEnvironment = "SYNARA_GIT_ASKPASS_SOCKET"

const (
	gitAskPassRequestBytes  = 512
	gitAskPassResponseBytes = 16 << 10
)

type gitAskPassEnvironment struct {
	Executable string
	SocketPath string
}

type gitAskPassServer struct {
	directory    string
	listener     net.Listener
	expectedHost string
	username     []byte
	token        []byte
	done         chan struct{}
	once         sync.Once
	workers      sync.WaitGroup
}

func newGitAskPassServer(temporaryRoot, expectedHost, username, token string) (*gitAskPassServer, error) {
	normalizedHost, err := gitpolicy.NormalizeHostname(expectedHost)
	if err != nil || strings.TrimSpace(username) == "" || token == "" {
		return nil, errors.New("Git AskPass Credential is incomplete")
	}
	directory, err := os.MkdirTemp(temporaryRoot, "synara-git-askpass-")
	if err != nil {
		return nil, fmt.Errorf("create Git AskPass directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("secure Git AskPass directory: %w", err)
	}
	socketPath := filepath.Join(directory, "credential.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("listen on Git AskPass socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("secure Git AskPass socket: %w", err)
	}
	server := &gitAskPassServer{
		directory: directory, listener: listener,
		expectedHost: normalizedHost, username: []byte(username), token: []byte(token), done: make(chan struct{}),
	}
	server.workers.Add(1)
	go server.serve()
	return server, nil
}

func (s *gitAskPassServer) Environment(executable string) (gitAskPassEnvironment, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" || !filepath.IsAbs(executable) {
		return gitAskPassEnvironment{}, errors.New("Git AskPass executable must be an absolute path")
	}
	return gitAskPassEnvironment{Executable: executable, SocketPath: filepath.Join(s.directory, "credential.sock")}, nil
}

func (s *gitAskPassServer) serve() {
	defer s.workers.Done()
	defer close(s.done)
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer connection.Close()
			s.handle(connection)
		}()
	}
}

func (s *gitAskPassServer) handle(connection net.Conn) {
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	request, err := io.ReadAll(io.LimitReader(connection, gitAskPassRequestBytes+1))
	if err != nil || len(request) > gitAskPassRequestBytes {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(string(request)), "\x00", 2)
	if len(parts) != 2 || parts[1] != s.expectedHost {
		return
	}
	var response []byte
	switch parts[0] {
	case "username":
		response = s.username
	case "password":
		response = s.token
	default:
		return
	}
	_, _ = connection.Write(response)
	_, _ = connection.Write([]byte{'\n'})
}

func (s *gitAskPassServer) Close() error {
	var closeErr error
	s.once.Do(func() {
		closeErr = s.listener.Close()
		<-s.done
		s.workers.Wait()
		zeroBytes(s.username)
		zeroBytes(s.token)
		s.username = nil
		s.token = nil
		if err := os.RemoveAll(s.directory); closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func RunGitAskPassHelperFromEnvironment(
	ctx context.Context,
	arguments []string,
	output io.Writer,
) (bool, error) {
	socketPath := strings.TrimSpace(os.Getenv(GitAskPassSocketEnvironment))
	if socketPath == "" {
		return false, nil
	}
	prompt := ""
	if len(arguments) > 1 {
		prompt = arguments[1]
	}
	return true, runGitAskPassHelper(ctx, socketPath, prompt, output)
}

func runGitAskPassHelper(ctx context.Context, socketPath, prompt string, output io.Writer) error {
	requestKind, promptHost, err := classifyGitAskPassPrompt(prompt)
	if err != nil {
		return err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return errors.New("Git AskPass socket is unavailable")
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(connection, requestKind+"\x00"+promptHost+"\n"); err != nil {
		return errors.New("Git AskPass request failed")
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		_ = unixConnection.CloseWrite()
	}
	response, err := io.ReadAll(io.LimitReader(connection, gitAskPassResponseBytes+2))
	if err != nil || len(response) == 0 || len(response) > gitAskPassResponseBytes+1 {
		return errors.New("Git AskPass response is invalid")
	}
	response = bytes.TrimSuffix(response, []byte{'\n'})
	defer zeroBytes(response)
	if len(response) == 0 {
		return errors.New("Git AskPass response is empty")
	}
	if _, err := output.Write(response); err != nil {
		return errors.New("Git AskPass output failed")
	}
	_, err = output.Write([]byte{'\n'})
	return err
}

func classifyGitAskPassPrompt(prompt string) (string, string, error) {
	normalizedPrompt := strings.ToLower(strings.TrimSpace(prompt))
	kind := ""
	switch {
	case strings.Contains(normalizedPrompt, "username"):
		kind = "username"
	case strings.Contains(normalizedPrompt, "password"):
		kind = "password"
	default:
		return "", "", errors.New("unsupported Git AskPass prompt")
	}
	start := strings.Index(normalizedPrompt, "https://")
	if start < 0 {
		return "", "", errors.New("Git AskPass prompt omitted an HTTPS repository")
	}
	candidate := normalizedPrompt[start:]
	if end := strings.IndexAny(candidate, "'\" \t\r\n"); end >= 0 {
		candidate = candidate[:end]
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.User == nil && kind == "password" || !strings.EqualFold(parsed.Scheme, "https") {
		return "", "", errors.New("Git AskPass prompt repository is invalid")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", "", errors.New("Git AskPass prompt uses an unsupported HTTPS port")
	}
	host, err := gitpolicy.NormalizeHostname(parsed.Hostname())
	if err != nil {
		return "", "", errors.New("Git AskPass prompt host is invalid")
	}
	return kind, host, nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
