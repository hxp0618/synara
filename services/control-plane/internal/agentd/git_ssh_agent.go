package agentd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type gitSSHAgentEnvironment struct {
	AuthSocket    string
	GitSSHCommand string
}

type gitSSHAgent struct {
	directory       string
	socketDirectory string
	socketPath      string
	listener        net.Listener
	keyring         agent.Agent
	done            chan struct{}
	workers         sync.WaitGroup
	once            sync.Once
}

func newGitSSHAgent(
	parent string,
	remote gitpolicy.Remote,
	credential GitSSHCredential,
) (*gitSSHAgent, error) {
	if remote.Scheme != "ssh" || remote.Hostname != credential.Host || remote.Username != credential.Username ||
		remote.Port != strconv.Itoa(credential.Port) || net.ParseIP(remote.PinnedIP) == nil {
		return nil, errors.New("Git SSH Credential does not match the pinned Repository endpoint")
	}
	parent, err := filepath.Abs(strings.TrimSpace(parent))
	if err != nil || parent == "" {
		return nil, errors.New("Git SSH temporary directory is invalid")
	}
	if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
		return nil, errors.New("Git SSH temporary directory is unavailable")
	}
	directory, err := os.MkdirTemp(parent, "synara-git-ssh-")
	if err != nil {
		return nil, errors.New("Git SSH temporary directory could not be created")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("Git SSH temporary directory permissions could not be applied")
	}

	privateKey := []byte(credential.PrivateKey)
	passphrase := []byte(credential.PrivateKeyPassphrase)
	defer zeroBytes(privateKey)
	defer zeroBytes(passphrase)
	var parsedPrivateKey any
	if len(passphrase) == 0 {
		parsedPrivateKey, err = ssh.ParseRawPrivateKey(privateKey)
	} else {
		parsedPrivateKey, err = ssh.ParseRawPrivateKeyWithPassphrase(privateKey, passphrase)
	}
	if err != nil {
		return nil, errors.New("Git SSH private key is invalid")
	}
	signer, err := ssh.NewSignerFromKey(parsedPrivateKey)
	if err != nil {
		return nil, errors.New("Git SSH private key is unsupported")
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: parsedPrivateKey}); err != nil {
		return nil, errors.New("Git SSH private key could not be loaded into the temporary agent")
	}

	publicKeyPath := filepath.Join(directory, "identity.pub")
	if err := os.WriteFile(publicKeyPath, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o600); err != nil {
		return nil, errors.New("Git SSH public identity could not be created")
	}
	hostKey, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(credential.HostKey))
	if err != nil || hostKey == nil || strings.TrimSpace(comment) != "" || len(options) != 0 ||
		len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("Git SSH pinned Host Key is invalid")
	}
	hostAlias := sshHostKeyAlias(remote.Hostname)
	knownHostsPath := filepath.Join(directory, "known_hosts")
	knownHosts := []byte(hostAlias + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))) + "\n")
	if err := os.WriteFile(knownHostsPath, knownHosts, 0o600); err != nil {
		return nil, errors.New("Git SSH pinned Host Key file could not be created")
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "synara-git-ssh-sock-")
	if err != nil {
		return nil, errors.New("Git SSH temporary agent socket directory could not be created")
	}
	defer func() {
		if cleanup {
			_ = os.RemoveAll(socketDirectory)
		}
	}()
	if err := os.Chmod(socketDirectory, 0o700); err != nil {
		return nil, errors.New("Git SSH temporary agent socket directory permissions could not be applied")
	}
	socketPath := filepath.Join(socketDirectory, "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, errors.New("Git SSH temporary agent socket could not be created")
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, errors.New("Git SSH temporary agent socket permissions could not be applied")
	}
	result := &gitSSHAgent{
		directory: directory, socketDirectory: socketDirectory, socketPath: socketPath,
		listener: listener, keyring: keyring, done: make(chan struct{}),
	}
	result.workers.Add(1)
	go result.serve()
	cleanup = false
	return result, nil
}

func (s *gitSSHAgent) Environment(remote gitpolicy.Remote) (gitSSHAgentEnvironment, error) {
	if s == nil || s.listener == nil || s.directory == "" || remote.Scheme != "ssh" ||
		net.ParseIP(remote.PinnedIP) == nil {
		return gitSSHAgentEnvironment{}, errors.New("Git SSH temporary agent is unavailable")
	}
	port, err := strconv.Atoi(remote.Port)
	if err != nil || port < 1 || port > 65535 {
		return gitSSHAgentEnvironment{}, errors.New("Git SSH Repository port is invalid")
	}
	publicKeyPath := filepath.Join(s.directory, "identity.pub")
	knownHostsPath := filepath.Join(s.directory, "known_hosts")
	hostAlias := sshHostKeyAlias(remote.Hostname)
	arguments := []string{
		"ssh", "-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "CanonicalizeHostname=no",
		"-o", "CheckHostIP=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HashKnownHosts=no",
		"-o", "HostKeyAlias=" + hostAlias,
		"-o", "HostName=" + remote.PinnedIP,
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=" + s.socketPath,
		"-o", "IdentityFile=" + publicKeyPath,
		"-o", "PasswordAuthentication=no",
		"-o", "PermitLocalCommand=no",
		"-o", "Port=" + remote.Port,
		"-o", "PreferredAuthentications=publickey",
		"-o", "ProxyCommand=none",
		"-o", "ProxyJump=none",
		"-o", "RequestTTY=no",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	return gitSSHAgentEnvironment{
		AuthSocket: s.socketPath, GitSSHCommand: strings.Join(quoted, " "),
	}, nil
}

func (s *gitSSHAgent) serve() {
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
			_ = agent.ServeAgent(s.keyring, connection)
		}()
	}
}

func (s *gitSSHAgent) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.once.Do(func() {
		if s.listener != nil {
			closeErr = s.listener.Close()
			<-s.done
		}
		s.workers.Wait()
		if s.keyring != nil {
			_ = s.keyring.RemoveAll()
		}
		if err := os.RemoveAll(s.directory); closeErr == nil {
			closeErr = err
		}
		if err := os.RemoveAll(s.socketDirectory); closeErr == nil {
			closeErr = err
		}
		s.listener = nil
		s.keyring = nil
		s.directory = ""
		s.socketDirectory = ""
		s.socketPath = ""
	})
	return closeErr
}

func sshHostKeyAlias(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s gitSSHAgentEnvironment) EnvironmentVariables() []string {
	if s.AuthSocket == "" || s.GitSSHCommand == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("SSH_AUTH_SOCK=%s", s.AuthSocket),
		fmt.Sprintf("GIT_SSH_COMMAND=%s", s.GitSSHCommand),
	}
}
