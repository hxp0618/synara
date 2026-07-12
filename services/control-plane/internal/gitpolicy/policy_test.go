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
