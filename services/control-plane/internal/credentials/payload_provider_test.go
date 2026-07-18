package credentials

import (
	"reflect"
	"testing"
)

func TestNormalizeManagedProviderCredentialPayloads(t *testing.T) {
	codex, _, err := normalizeCredentialPayload(
		PurposeProvider,
		ProviderCodex,
		ProviderAPIKeyCredentialType,
		map[string]any{
			"apiKey":  "provider-secret",
			"baseUrl": " https://api.example.test/v1/ ",
		},
	)
	if err != nil {
		t.Fatalf("valid Codex provider payload was rejected: %v", err)
	}
	if codex["apiKey"] != "provider-secret" || codex["baseUrl"] != "https://api.example.test/v1" {
		t.Fatalf("Codex provider payload was not normalized: %#v", codex)
	}

	claude, _, err := normalizeCredentialPayload(
		PurposeProvider,
		ProviderClaudeAgent,
		ProviderAPIKeyCredentialType,
		map[string]any{"apiKey": "claude-secret"},
	)
	if err != nil || claude["apiKey"] != "claude-secret" {
		t.Fatalf("valid Claude provider payload was rejected: %v %#v", err, claude)
	}
}

func TestNormalizeManagedProviderCredentialPayloadRejectsUnsupportedFields(t *testing.T) {
	tests := []struct {
		name           string
		provider       string
		credentialType string
		payload        map[string]any
	}{
		{
			name:           "codex organization rejected",
			provider:       ProviderCodex,
			credentialType: ProviderAPIKeyCredentialType,
			payload: map[string]any{
				"apiKey":       "provider-secret",
				"organization": "org_123",
			},
		},
		{
			name:           "claude auth token rejected",
			provider:       ProviderClaudeAgent,
			credentialType: ProviderAPIKeyCredentialType,
			payload:        map[string]any{"authToken": "claude-secret"},
		},
		{
			name:           "managed provider type rejected",
			provider:       ProviderCodex,
			credentialType: "oauth",
			payload:        map[string]any{"apiKey": "provider-secret"},
		},
		{
			name:           "unsafe base url rejected",
			provider:       ProviderClaudeAgent,
			credentialType: ProviderAPIKeyCredentialType,
			payload: map[string]any{
				"apiKey":  "claude-secret",
				"baseUrl": "https://user:password@example.test/v1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := normalizeCredentialPayload(
				PurposeProvider,
				test.provider,
				test.credentialType,
				test.payload,
			); err == nil {
				t.Fatal("invalid managed provider payload was accepted")
			}
		})
	}
}

func TestNormalizeResolvedProviderPayloadKeepsLegacyManagedFields(t *testing.T) {
	codex, _, err := normalizeResolvedCredentialPayload(
		PurposeProvider,
		ProviderCodex,
		ProviderAPIKeyCredentialType,
		map[string]any{
			"apiKey":       "provider-secret",
			"baseUrl":      "https://api.example.test/v1/",
			"organization": " org_123 ",
		},
	)
	if err != nil {
		t.Fatalf("legacy Codex provider payload was rejected during resolve: %v", err)
	}
	if codex["organization"] != "org_123" || codex["baseUrl"] != "https://api.example.test/v1" {
		t.Fatalf("legacy Codex provider payload was not normalized: %#v", codex)
	}

	claude, _, err := normalizeResolvedCredentialPayload(
		PurposeProvider,
		ProviderClaudeAgent,
		ProviderAPIKeyCredentialType,
		map[string]any{
			"authToken": "claude-secret",
			"baseUrl":   "https://claude.example.test/",
		},
	)
	if err != nil {
		t.Fatalf("legacy Claude provider payload was rejected during resolve: %v", err)
	}
	if claude["authToken"] != "claude-secret" || claude["baseUrl"] != "https://claude.example.test" {
		t.Fatalf("legacy Claude provider payload was not normalized: %#v", claude)
	}
}

func TestNormalizeProviderPayloadPassesThroughAdvancedProviders(t *testing.T) {
	payload := map[string]any{"clientId": "client-id", "clientSecret": "client-secret"}
	normalized, _, err := normalizeCredentialPayload(
		PurposeProvider,
		"cursor",
		"oauth",
		payload,
	)
	if err != nil {
		t.Fatalf("advanced provider payload was rejected: %v", err)
	}
	if !reflect.DeepEqual(normalized, payload) {
		t.Fatalf("advanced provider payload should remain opaque: %#v", normalized)
	}
}
