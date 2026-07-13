package credentials

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	PurposeProvider = "provider"
	PurposeGit      = "git"

	GitProvider            = "git"
	GitHTTPSCredentialType = "https_token"
)

type GitHTTPSPayload struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func normalizePurpose(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = PurposeProvider
	}
	if value != PurposeProvider && value != PurposeGit {
		return "", problem.New(400, "invalid_credential_purpose", "Credential purpose must be provider or git.")
	}
	return value, nil
}

func normalizeCredentialPayload(
	purpose, provider, credentialType string,
	payload map[string]any,
) (map[string]any, []byte, error) {
	if purpose != PurposeGit {
		encoded, err := encodePayload(payload)
		return payload, encoded, err
	}
	if provider != GitProvider || credentialType != GitHTTPSCredentialType {
		return nil, nil, problem.New(
			400,
			"invalid_git_credential_type",
			"Git Credentials currently support only provider git with credential type https_token.",
		)
	}
	normalized, err := normalizeGitHTTPSPayload(payload)
	if err != nil {
		return nil, nil, err
	}
	value := map[string]any{"host": normalized.Host, "username": normalized.Username, "token": normalized.Token}
	encoded, err := encodePayload(value)
	return value, encoded, err
}

func normalizeGitHTTPSPayload(payload map[string]any) (GitHTTPSPayload, error) {
	if len(payload) != 3 {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	host, hostOK := payload["host"].(string)
	username, usernameOK := payload["username"].(string)
	token, tokenOK := payload["token"].(string)
	if !hostOK || !usernameOK || !tokenOK {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	normalizedHost, err := normalizeGitHost(host)
	if err != nil {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 512 || containsControl(username) {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	if token == "" || len(token) > 16<<10 || containsControl(token) {
		return GitHTTPSPayload{}, invalidGitCredentialPayload()
	}
	return GitHTTPSPayload{Host: normalizedHost, Username: username, Token: token}, nil
}

func decodeGitHTTPSPayload(payload map[string]any) (GitHTTPSPayload, error) {
	return normalizeGitHTTPSPayload(payload)
}

func normalizeGitHost(value string) (string, error) {
	normalized, err := gitpolicy.NormalizeHostname(value)
	if err != nil {
		return "", problem.New(400, "invalid_git_credential_host", "Git Credential host is invalid.")
	}
	return normalized, nil
}

func encodePayload(payload map[string]any) ([]byte, error) {
	if len(payload) == 0 {
		return nil, problem.New(400, "invalid_credential_payload", "Credential payload must not be empty.")
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxCredentialPayloadBytes {
		return nil, problem.New(400, "invalid_credential_payload", "Credential payload must be valid JSON no larger than 65536 bytes.")
	}
	return encoded, nil
}

func invalidGitCredentialPayload() error {
	return problem.New(
		400,
		"invalid_git_credential_payload",
		"Git https_token payload must contain only non-empty host, username, and token strings.",
	)
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
