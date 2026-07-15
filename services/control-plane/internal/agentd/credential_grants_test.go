package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestResolveCredentialGrantUsesOpaqueGrantAndCurrentLease(t *testing.T) {
	executionID := uuid.New()
	grantID := uuid.New()
	tenantID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/v1/workers/executions/"+executionID.String()+"/credential-grants/"+grantID.String()+"/resolve" {
			t.Fatalf("unexpected Credential Grant request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer worker-token" {
			t.Fatalf("Credential Grant request omitted Worker authentication: %q", request.Header.Get("Authorization"))
		}
		var lease executions.LeaseInput
		if err := json.NewDecoder(request.Body).Decode(&lease); err != nil {
			t.Fatal(err)
		}
		if lease.TenantID != tenantID || lease.Generation != 7 || lease.LeaseToken != "lease-token" {
			t.Fatalf("Credential Grant request used the wrong Lease: %#v", lease)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(ResolvedWorkspaceCredential{
			GrantID: grantID, BindingKind: "registry_pull", Purpose: "registry",
			Provider: "oci", CredentialType: "bearer_token", Selector: "registry.example.com",
			Payload: map[string]any{"host": "registry.example.com", "token": "registry-secret"},
		})
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"

	resolved, err := client.ResolveCredentialGrant(context.Background(), executionID, grantID, executions.Lease{
		TenantID: tenantID, Generation: 7, LeaseToken: "lease-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GrantID != grantID || resolved.BindingKind != "registry_pull" ||
		resolved.Payload["token"] != "registry-secret" {
		t.Fatalf("unexpected resolved Credential Grant: %#v", resolved)
	}
}

func TestResolveCredentialStageResolvesOnlyRequestedDescriptor(t *testing.T) {
	executionID := uuid.New()
	gitGrantID := uuid.New()
	packageGrantID := uuid.New()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/workers/executions/"+executionID.String()+
			"/credential-grants/"+gitGrantID.String()+"/resolve" {
			t.Fatalf("unexpected stage Credential request: %s", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(ResolvedWorkspaceCredential{
			GrantID: gitGrantID, BindingKind: "git_fetch", Purpose: "git", Provider: "git",
			CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
			Payload: map[string]any{
				"host": "git.example.com", "username": "synara", "token": "git-secret-token",
			},
		})
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{ControlPlaneURL: baseURL, RequestTimeout: time.Second, ArtifactTimeout: time.Second})
	client.workerToken = "worker-token"
	grants := []executions.CredentialGrantDescriptor{
		{
			GrantID: packageGrantID, BindingKind: "package_read", Purpose: "package", Provider: "npm",
			CredentialType: "npm_token", Selector: "https://registry.example.com/",
		},
		{
			GrantID: gitGrantID, BindingKind: "git_fetch", Purpose: "git", Provider: "git",
			CredentialType: "https_token", Selector: "https://git.example.com/team/repository.git",
		},
	}
	resolved, err := client.ResolveCredentialStage(
		context.Background(), executionID,
		executions.Lease{TenantID: uuid.New(), Generation: 3, LeaseToken: "lease-token"},
		grants, "git_fetch",
	)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || resolved == nil || resolved.GrantID != gitGrantID {
		t.Fatalf("controlled stage resolution: requests=%d resolved=%#v", requests, resolved)
	}
	clearResolvedWorkspaceCredential(resolved)
	missing, err := client.ResolveCredentialStage(
		context.Background(), executionID,
		executions.Lease{TenantID: uuid.New(), Generation: 3, LeaseToken: "lease-token"},
		grants, "registry_pull",
	)
	if err != nil || missing != nil || requests != 1 {
		t.Fatalf("absent stage was resolved eagerly: requests=%d missing=%#v err=%v", requests, missing, err)
	}
}

func TestResolveCredentialStageRejectsInfrastructureOrInvalidDescriptorsBeforeNetwork(t *testing.T) {
	client := &Client{}
	for _, descriptor := range []executions.CredentialGrantDescriptor{
		{
			GrantID: uuid.New(), BindingKind: "worker_image_pull", Purpose: "registry", Provider: "oci",
			CredentialType: "bearer_token", Selector: "registry.example.com",
		},
		{
			GrantID: uuid.New(), BindingKind: "package_read", Purpose: "provider", Provider: "npm",
			CredentialType: "npm_token", Selector: "https://registry.example.com/",
		},
	} {
		if _, err := client.ResolveCredentialStage(
			context.Background(), uuid.New(), executions.Lease{},
			[]executions.CredentialGrantDescriptor{descriptor}, descriptor.BindingKind,
		); err == nil {
			t.Fatalf("invalid stage descriptor was accepted: %#v", descriptor)
		}
	}
}
