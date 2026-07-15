package gitpolicy

import (
	"context"
	"net"
	"testing"
)

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return r[host], nil
}

func TestValidateRemoteHTTPSRejectsCredentialAndSSRFTargets(t *testing.T) {
	resolver := staticResolver{
		"git.example.com":  {{IP: net.ParseIP("93.184.216.34")}},
		"internal.example": {{IP: net.ParseIP("10.0.0.8")}},
	}
	for _, candidate := range []string{
		"file:///tmp/repository", "ssh://git@git.example.com/repository.git",
		"https://token@git.example.com/repository.git", "https://git.example.com/repository.git?token=secret",
		"https://localhost/repository.git", "https://127.0.0.1/repository.git",
		"https://169.254.169.254/latest/meta-data", "https://internal.example/repository.git",
		"https://git.example.com/../repository.git", "https://git.example.com:99999/repository.git",
	} {
		if _, err := ValidateRemoteHTTPS(context.Background(), resolver, candidate); err == nil {
			t.Fatalf("unsafe Repository URL was accepted: %s", candidate)
		}
	}
	valid, err := ValidateRemoteHTTPS(context.Background(), resolver, "HTTPS://GIT.EXAMPLE.COM/team/repository.git")
	if err != nil {
		t.Fatal(err)
	}
	if valid != "https://git.example.com/team/repository.git" {
		t.Fatalf("unexpected canonical Repository URL %q", valid)
	}
	remote, err := ResolveRemoteHTTPS(context.Background(), resolver, "https://git.example.com/team/repository.git")
	if err != nil || remote.Hostname != "git.example.com" || remote.Port != "443" || remote.PinnedIP != "93.184.216.34" {
		t.Fatalf("Repository endpoint was not safely pinned: remote=%#v err=%v", remote, err)
	}
}

func TestValidateRemoteSSHRequiresUserAndPinsPublicEndpoint(t *testing.T) {
	resolver := staticResolver{
		"git.example.com":  {{IP: net.ParseIP("93.184.216.34")}},
		"internal.example": {{IP: net.ParseIP("10.0.0.8")}},
	}
	for _, candidate := range []string{
		"git@git.example.com:team/repository.git",
		"ssh://git.example.com/team/repository.git",
		"ssh://git:password@git.example.com/team/repository.git",
		"ssh://git%20user@git.example.com/team/repository.git",
		"ssh://git@localhost/team/repository.git",
		"ssh://git@127.0.0.1/team/repository.git",
		"ssh://git@internal.example/team/repository.git",
		"ssh://git@git.example.com/../repository.git",
		"ssh://git@git.example.com/team/repository.git?token=secret",
		"ssh://git@git.example.com:99999/team/repository.git",
	} {
		if _, err := ValidateRemoteSSH(context.Background(), resolver, candidate); err == nil {
			t.Fatalf("unsafe SSH Repository URL was accepted: %s", candidate)
		}
	}
	remote, err := ResolveRemoteSSH(
		context.Background(), resolver, "SSH://git@GIT.EXAMPLE.COM:2222/team/repository.git",
	)
	if err != nil {
		t.Fatal(err)
	}
	if remote.URL != "ssh://git@git.example.com:2222/team/repository.git" || remote.Scheme != "ssh" ||
		remote.Username != "git" || remote.Hostname != "git.example.com" || remote.Port != "2222" ||
		remote.PinnedIP != "93.184.216.34" {
		t.Fatalf("unexpected canonical SSH Remote: %#v", remote)
	}
}

func TestResolveRemoteDispatchesOnlyExplicitHTTPSOrSSH(t *testing.T) {
	resolver := staticResolver{"git.example.com": {{IP: net.ParseIP("93.184.216.34")}}}
	for _, candidate := range []struct {
		value  string
		scheme string
	}{
		{value: "https://git.example.com/team/repository.git", scheme: "https"},
		{value: "ssh://git@git.example.com/team/repository.git", scheme: "ssh"},
	} {
		remote, err := ResolveRemote(context.Background(), resolver, candidate.value)
		if err != nil || remote.Scheme != candidate.scheme {
			t.Fatalf("ResolveRemote(%q) = %#v, %v", candidate.value, remote, err)
		}
	}
	for _, value := range []string{
		"git@git.example.com:team/repository.git", "git://git.example.com/team/repository.git",
		"file:///tmp/repository.git", "https:opaque",
	} {
		if _, err := ResolveRemote(context.Background(), resolver, value); err == nil {
			t.Fatalf("unsupported Repository syntax was accepted: %q", value)
		}
	}
}

func TestNormalizeBranchRejectsAmbiguousRefs(t *testing.T) {
	for _, branch := range []string{"../main", "main lock", "refs//heads/main", "main.lock", "@{upstream}", "-main", "/main", "main/", "@"} {
		if _, err := NormalizeBranch(branch, "main"); err == nil {
			t.Fatalf("unsafe branch was accepted: %s", branch)
		}
	}
	branch, err := NormalizeBranch("", "main")
	if err != nil || branch != "main" {
		t.Fatalf("unexpected default branch: branch=%q err=%v", branch, err)
	}
}
