package agentd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestGitSSHAgentUsesOneEphemeralKeyAndPinnedEndpoint(t *testing.T) {
	privateKey, hostKey := testGitSSHKeyPair(t, "temporary-passphrase")
	credential := GitSSHCredential{
		Host: "git.example.com", Port: 2222, Username: "git", PrivateKey: privateKey,
		PrivateKeyPassphrase: "temporary-passphrase", HostKey: hostKey,
	}
	remote := gitpolicy.Remote{
		URL: "ssh://git@git.example.com:2222/team/repository.git", Scheme: "ssh",
		Hostname: "git.example.com", Port: "2222", PinnedIP: "93.184.216.34", Username: "git",
	}
	temporaryRoot := t.TempDir()
	server, err := newGitSSHAgent(temporaryRoot, remote, credential)
	if err != nil {
		t.Fatal(err)
	}
	directory := server.directory
	environment, err := server.Environment(remote)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(environment.GitSSHCommand, "temporary-passphrase") ||
		strings.Contains(environment.GitSSHCommand, "OPENSSH PRIVATE KEY") ||
		!strings.Contains(environment.GitSSHCommand, "HostName=93.184.216.34") ||
		!strings.Contains(environment.GitSSHCommand, "HostKeyAlias=git.example.com") ||
		!strings.Contains(environment.GitSSHCommand, "StrictHostKeyChecking=yes") {
		t.Fatalf("unsafe Git SSH command: %s", environment.GitSSHCommand)
	}
	variables := environment.EnvironmentVariables()
	if len(variables) != 2 || variables[0] != "SSH_AUTH_SOCK="+environment.AuthSocket {
		t.Fatalf("unexpected Git SSH environment: %#v", variables)
	}
	knownHosts, err := os.ReadFile(filepath.Join(directory, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(knownHosts), "git.example.com ssh-ed25519 ") {
		t.Fatalf("unexpected pinned Host Key file: %q", knownHosts)
	}

	connection, err := net.Dial("unix", environment.AuthSocket)
	if err != nil {
		t.Fatal(err)
	}
	client := agent.NewClient(connection)
	keys, err := client.List()
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if len(keys) != 1 {
		_ = connection.Close()
		t.Fatalf("temporary SSH agent keys = %d, want 1", len(keys))
	}
	publicKey, err := ssh.ParsePublicKey(keys[0].Blob)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	payload := []byte("synara-git-ssh-stage")
	signature, err := client.Sign(publicKey, payload)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := publicKey.Verify(payload, signature); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.Close()

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("Git SSH temporary directory survived cleanup: %v", err)
	}
}

func TestGitSSHAgentRejectsCredentialEndpointMismatch(t *testing.T) {
	privateKey, hostKey := testGitSSHKeyPair(t, "")
	credential := GitSSHCredential{
		Host: "other.example.com", Port: 22, Username: "git", PrivateKey: privateKey, HostKey: hostKey,
	}
	remote := gitpolicy.Remote{
		Scheme: "ssh", Hostname: "git.example.com", Port: "22", PinnedIP: "93.184.216.34", Username: "git",
	}
	if _, err := newGitSSHAgent(t.TempDir(), remote, credential); err == nil {
		t.Fatal("Git SSH Credential was accepted for another endpoint")
	}
}

func TestGitSSHAgentEnvironmentAuthenticatesAgainstPinnedHost(t *testing.T) {
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientBlock, err := ssh.MarshalPrivateKey(clientPrivate, "")
	if err != nil {
		t.Fatal(err)
	}
	clientSSHKey, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErrors := make(chan error, 1)
	go servePinnedSSHTest(listener, clientSSHKey.Marshal(), hostSigner, serverErrors)

	port := listener.Addr().(*net.TCPAddr).Port
	credential := GitSSHCredential{
		Host: "git.example.com", Port: port, Username: "git",
		PrivateKey: string(pem.EncodeToMemory(clientBlock)),
		HostKey:    string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())),
	}
	remote := gitpolicy.Remote{
		URL:    "ssh://git@git.example.com:" + strconv.Itoa(port) + "/team/repository.git",
		Scheme: "ssh", Hostname: "git.example.com", Port: strconv.Itoa(port),
		PinnedIP: "127.0.0.1", Username: "git",
	}
	server, err := newGitSSHAgent(t.TempDir(), remote, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	environment, err := server.Environment(remote)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"/bin/sh", "-c",
		environment.GitSSHCommand+" "+shellQuote("git@"+remote.Hostname)+" "+shellQuote("true"),
	)
	command.Env = []string{"PATH=/usr/bin:/bin", "SSH_AUTH_SOCK=" + environment.AuthSocket}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("pinned SSH authentication failed: %v: %s", err, stderr.String())
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pinned SSH test server did not finish")
	}
}

func TestShellQuotePreservesLiteralArguments(t *testing.T) {
	value := "/tmp/path with 'quotes'"
	quoted := shellQuote(value)
	if quoted == value || !bytes.Contains([]byte(quoted), []byte(`'"'"'`)) {
		t.Fatalf("argument was not safely shell-quoted: %q", quoted)
	}
}

func servePinnedSSHTest(
	listener net.Listener,
	expectedClientKey []byte,
	hostKey ssh.Signer,
	result chan<- error,
) {
	connection, err := listener.Accept()
	if err != nil {
		result <- err
		return
	}
	defer connection.Close()
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != "git" || !bytes.Equal(key.Marshal(), expectedClientKey) {
				return nil, fmt.Errorf("unexpected SSH identity")
			}
			return nil, nil
		},
	}
	config.AddHostKey(hostKey)
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		result <- err
		return
	}
	defer serverConnection.Close()
	go ssh.DiscardRequests(requests)
	for pending := range channels {
		if pending.ChannelType() != "session" {
			_ = pending.Reject(ssh.UnknownChannelType, "unsupported channel")
			continue
		}
		channel, channelRequests, acceptErr := pending.Accept()
		if acceptErr != nil {
			result <- acceptErr
			return
		}
		for request := range channelRequests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			if len(request.Payload) < 4 || int(binary.BigEndian.Uint32(request.Payload[:4])) != len(request.Payload)-4 {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			_, _ = channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			_ = channel.Close()
			result <- nil
			return
		}
	}
	result <- fmt.Errorf("SSH client did not open a session")
}
