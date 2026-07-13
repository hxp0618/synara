package agentd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitAskPassServerHandlesRepeatedPromptsAndCleansUp(t *testing.T) {
	server, err := newGitAskPassServer("/tmp", "git.example.com", "x-access-token", "private-token-value")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := server.Environment("/usr/local/bin/synara-agentd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(environment.SocketPath, "private-token-value") || strings.Contains(environment.Executable, "private-token-value") {
		t.Fatal("Git Credential leaked into AskPass environment metadata")
	}
	for _, test := range []struct {
		prompt string
		want   string
	}{
		{prompt: "Username for 'https://git.example.com':", want: "x-access-token\n"},
		{prompt: "Password for 'https://x-access-token@git.example.com':", want: "private-token-value\n"},
		{prompt: "Password for 'https://x-access-token@git.example.com':", want: "private-token-value\n"},
	} {
		var output bytes.Buffer
		if err := runGitAskPassHelper(context.Background(), environment.SocketPath, test.prompt, &output); err != nil {
			t.Fatal(err)
		}
		if output.String() != test.want {
			t.Fatalf("AskPass output = %q, want %q", output.String(), test.want)
		}
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(environment.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("AskPass socket was not removed: %v", err)
	}
	if server.username != nil || server.token != nil {
		t.Fatal("AskPass Credential buffers were retained after close")
	}
	var postClose bytes.Buffer
	if err := runGitAskPassHelper(
		context.Background(), environment.SocketPath, "Password for 'https://x-access-token@git.example.com':", &postClose,
	); err == nil || postClose.Len() != 0 {
		t.Fatalf("closed AskPass server still returned a Credential: output=%q err=%v", postClose.String(), err)
	}
}

func TestGitAskPassHelperModeUsesOnlySocketPathFromEnvironment(t *testing.T) {
	server, err := newGitAskPassServer("/tmp", "git.example.com", "git-user", "git-token")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	environment, err := server.Environment("/usr/local/bin/synara-agentd")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(GitAskPassSocketEnvironment, environment.SocketPath)
	var output bytes.Buffer
	handled, err := RunGitAskPassHelperFromEnvironment(
		context.Background(), []string{"synara-agentd", "Password for 'https://git-user@git.example.com':"}, &output,
	)
	if err != nil || !handled {
		t.Fatalf("AskPass helper mode was not handled: handled=%t err=%v", handled, err)
	}
	if output.String() != "git-token\n" {
		t.Fatalf("unexpected helper output %q", output.String())
	}

	var rejected bytes.Buffer
	_, err = RunGitAskPassHelperFromEnvironment(
		context.Background(), []string{"synara-agentd", "One-time code:"}, &rejected,
	)
	if err == nil || rejected.Len() != 0 {
		t.Fatalf("unknown AskPass prompt was not rejected safely: output=%q err=%v", rejected.String(), err)
	}
	_, err = RunGitAskPassHelperFromEnvironment(
		context.Background(), []string{"synara-agentd", "Password for 'https://git-user@evil.example.com':"}, &rejected,
	)
	if err == nil || rejected.Len() != 0 {
		t.Fatalf("cross-host AskPass prompt was not rejected safely: output=%q err=%v", rejected.String(), err)
	}
}

func TestGitAskPassEnvironmentContainsNoCredentialValue(t *testing.T) {
	server, err := newGitAskPassServer("/tmp", "git.example.com", "git-user", "do-not-leak-token")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	metadata, err := server.Environment("/usr/local/bin/synara-agentd")
	if err != nil {
		t.Fatal(err)
	}
	environment := strings.Join(gitEnvironment(&metadata), "\n")
	if strings.Contains(environment, "git-user") || strings.Contains(environment, "do-not-leak-token") {
		t.Fatalf("Git Credential leaked into process environment: %s", environment)
	}
	for _, required := range []string{
		"GIT_ASKPASS=/usr/local/bin/synara-agentd",
		"GIT_ASKPASS_REQUIRE=force",
		GitAskPassSocketEnvironment + "=" + metadata.SocketPath,
	} {
		if !strings.Contains(environment, required) {
			t.Fatalf("AskPass environment omitted %s: %s", required, environment)
		}
	}
	if filepath.Dir(metadata.SocketPath) == "." {
		t.Fatalf("AskPass socket path is not absolute: %s", metadata.SocketPath)
	}
}
