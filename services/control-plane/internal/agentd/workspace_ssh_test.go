package agentd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

func TestWorkspaceNetworkGitFetchesThroughEphemeralPinnedSSHAgent(t *testing.T) {
	source := createWorkspaceTestSource(t)
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
	go serveGitUploadPackSSH(listener, clientSSHKey.Marshal(), hostSigner, source, serverErrors)

	port := listener.Addr().(*net.TCPAddr).Port
	remote := gitpolicy.Remote{
		URL:    "ssh://git@git.example.com:" + strconv.Itoa(port) + "/team/repository.git",
		Scheme: "ssh", Hostname: "git.example.com", Port: strconv.Itoa(port),
		PinnedIP: "127.0.0.1", Username: "git",
	}
	credential := &WorkspaceGitCredential{SSH: &GitSSHCredential{
		Host: "git.example.com", Port: port, Username: "git",
		PrivateKey: string(pem.EncodeToMemory(clientBlock)),
		HostKey:    string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())),
	}}
	temporaryRoot := t.TempDir()
	t.Setenv("TMPDIR", temporaryRoot)
	materializer := NewWorkspaceMaterializerWithCache(t.TempDir(), t.TempDir(), uuid.New())
	repository := t.TempDir()
	runTestGit(t, repository, "init", "--bare", "--initial-branch=main")
	runTestGit(t, repository, "remote", "add", "origin", remote.URL)
	if _, err := materializer.runNetworkGit(
		context.Background(), repository, remote, credential,
		"fetch", "--prune", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main",
	); err != nil {
		t.Fatal(err)
	}
	commit := runTestGitOutput(t, repository, "rev-parse", "refs/remotes/origin/main^{commit}")
	if !validGitObjectID(strings.TrimSpace(commit)) {
		t.Fatalf("SSH Fetch did not install the remote branch: %q", commit)
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Git SSH test server did not finish")
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Git SSH temporary credential files survived: %#v", entries)
	}
	privateMarker := []byte("OPENSSH PRIVATE KEY")
	err = filepath.WalkDir(repository, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(content, privateMarker) {
			return fmt.Errorf("private key was written to Git storage")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func serveGitUploadPackSSH(
	listener net.Listener,
	expectedClientKey []byte,
	hostKey ssh.Signer,
	repository string,
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
			if request.Type != "exec" || len(request.Payload) < 4 ||
				int(binary.BigEndian.Uint32(request.Payload[:4])) != len(request.Payload)-4 ||
				!strings.HasPrefix(string(request.Payload[4:]), "git-upload-pack ") {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			command := exec.Command("git", "upload-pack", repository)
			command.Stdin = channel
			command.Stdout = channel
			command.Stderr = io.Discard
			commandErr := command.Run()
			status := uint32(0)
			if commandErr != nil {
				status = 1
			}
			statusBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(statusBytes, status)
			_, _ = channel.SendRequest("exit-status", false, statusBytes)
			_ = channel.Close()
			result <- commandErr
			return
		}
	}
	result <- fmt.Errorf("Git SSH client did not open a session")
}
