package agentd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

func TestExecutionSecretGuardRegistersOnlyExplicitCredentialSources(t *testing.T) {
	resumeCursor := "resume-cursor-123456"
	gitToken := "git-token-123456789"
	apiKey := "provider-api-key-123456"
	authToken := "provider-auth-token-123456"
	unknownValue := "unknown-provider-setting-123456"
	workerToken := "worker-token-123456789"
	leaseToken := "lease-token-123456789"

	guard, err := newExecutionSecretGuard(&resumeCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := guard.AddGitCredential(&GitHTTPSCredential{
		Username: "git-user", Token: gitToken,
	}); err != nil {
		t.Fatal(err)
	}
	if err := guard.AddProviderCredential(&RunnerCredential{Payload: map[string]any{
		"apiKey": apiKey, "authToken": authToken,
		"baseUrl": unknownValue, "organization": unknownValue,
	}}); err != nil {
		t.Fatal(err)
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("git-user:"+gitToken))
	sanitized, err := guard.SanitizeMap(map[string]any{
		"resume": "cursor=" + resumeCursor,
		"git":    basic,
		"provider": []any{
			"api=" + apiKey,
			map[string]any{"auth": authToken},
		},
		"unknown": map[string]any{
			"baseUrl": unknownValue, "organization": unknownValue,
			"workerToken": workerToken, "leaseToken": leaseToken,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := mustJSONForSecretGuardTest(t, sanitized)
	for _, secret := range []string{resumeCursor, gitToken, apiKey, authToken, basic} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("registered secret remained in sanitized payload: %q", secret)
		}
	}
	if !strings.Contains(encoded, secretguard.RedactionMarker) {
		t.Fatalf("sanitized payload omitted redaction marker: %s", encoded)
	}
	for _, ordinaryValue := range []string{unknownValue, workerToken, leaseToken} {
		if !strings.Contains(encoded, ordinaryValue) {
			t.Fatalf("non-registered value was unexpectedly changed: %q in %s", ordinaryValue, encoded)
		}
	}

	_, err = guard.SanitizeMap(map[string]any{"unsafe-" + apiKey: "value"})
	if err == nil || runnerFailureCode(err) != secretguard.ErrorCode || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("unsafe structural key error = %v, code = %q", err, runnerFailureCode(err))
	}
}

func TestExecutionSecretGuardRegistersWorkspaceCredentialVariants(t *testing.T) {
	guard, err := newExecutionSecretGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	workspace := &WorkspaceGitCredential{
		HTTPS: &GitHTTPSCredential{Username: "git-user", Token: "git-workspace-secret"},
		SSH: &GitSSHCredential{
			PrivateKey:           "-----BEGIN OPENSSH PRIVATE KEY-----\nworkspace-private-key-secret\n-----END OPENSSH PRIVATE KEY-----",
			PrivateKeyPassphrase: "workspace-passphrase-secret",
		},
	}
	registry := &RegistryCredential{
		CredentialType: "basic", Username: "registry-user", Password: "registry-password-secret",
	}
	pypi := &PackageCredential{
		Provider: "pypi", Username: "__token__", Token: "pypi-package-secret",
	}
	for _, register := range []func() error{
		func() error { return guard.AddWorkspaceGitCredential(workspace) },
		func() error { return guard.AddRegistryCredential(registry) },
		func() error { return guard.AddPackageCredential(pypi) },
	} {
		if err := register(); err != nil {
			t.Fatal(err)
		}
	}
	values := []string{
		workspace.HTTPS.Token,
		"Basic " + base64.StdEncoding.EncodeToString([]byte("git-user:"+workspace.HTTPS.Token)),
		workspace.SSH.PrivateKey, workspace.SSH.PrivateKeyPassphrase,
		registry.Password,
		"Basic " + base64.StdEncoding.EncodeToString([]byte("registry-user:"+registry.Password)),
		pypi.Token,
		"Basic " + base64.StdEncoding.EncodeToString([]byte("__token__:"+pypi.Token)),
	}
	for _, value := range values {
		sanitized, err := guard.SanitizeMap(map[string]any{"value": "before " + value + " after"})
		encoded := mustJSONForSecretGuardTest(t, sanitized)
		if err != nil || strings.Contains(encoded, value) ||
			!strings.Contains(encoded, secretguard.RedactionMarker) {
			t.Fatalf("Workspace Credential variant was not guarded: value-length=%d sanitized=%q err=%v", len(value), encoded, err)
		}
	}
}

func TestExecutionSecretGuardSanitizesResultAndKeepsCursorOnDedicatedPath(t *testing.T) {
	apiKey := "provider-api-key-654321"
	cursor := "provider-cursor-654321"
	guard, err := newExecutionSecretGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := guard.AddProviderCredential(&RunnerCredential{Payload: map[string]any{"apiKey": apiKey}}); err != nil {
		t.Fatal(err)
	}

	result, err := guard.SanitizeResult(RunnerResult{
		ProviderResumeCursor: &cursor,
		Output: map[string]any{
			"cursorCopy": cursor,
			"message":    "used " + apiKey,
		},
		PrimaryOperationResult: map[string]any{
			"providerResumeCursor": cursor,
			"output": map[string]any{
				"cursorCopy": cursor,
				"message":    "used " + apiKey,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderResumeCursor == nil || *result.ProviderResumeCursor != cursor {
		t.Fatalf("dedicated Provider Cursor = %#v, want %q", result.ProviderResumeCursor, cursor)
	}
	if result.PrimaryOperationResult["providerResumeCursor"] != cursor {
		t.Fatalf("primary operation Cursor = %#v, want %q", result.PrimaryOperationResult["providerResumeCursor"], cursor)
	}
	for name, payload := range map[string]map[string]any{
		"output":  result.Output,
		"primary": result.PrimaryOperationResult["output"].(map[string]any),
	} {
		encoded := mustJSONForSecretGuardTest(t, payload)
		if strings.Contains(encoded, cursor) || strings.Contains(encoded, apiKey) ||
			!strings.Contains(encoded, secretguard.RedactionMarker) {
			t.Fatalf("%s payload was not sanitized: %s", name, encoded)
		}
	}

	failure := &runnerFailure{code: "provider_failed", message: "Provider stderr contained " + apiKey}
	sanitizedFailure := guard.SanitizeError(failure)
	if runnerFailureCode(sanitizedFailure) != "provider_failed" ||
		strings.Contains(sanitizedFailure.Error(), apiKey) ||
		!strings.Contains(sanitizedFailure.Error(), secretguard.RedactionMarker) {
		t.Fatalf("sanitized failure = %T %v", sanitizedFailure, sanitizedFailure)
	}
	revoked := guard.SanitizeError(&controlPlaneProblem{
		Code: "worker_token_revoked", Status: 401, Message: "revoked after " + apiKey,
	})
	if !isWorkerRevocationError(revoked) || strings.Contains(revoked.Error(), apiKey) ||
		!strings.Contains(revoked.Error(), secretguard.RedactionMarker) {
		t.Fatalf("sanitized revocation = %T %v", revoked, revoked)
	}
}

func TestExecutionSecretGuardAddsControlCursorWithoutClosingActiveStreams(t *testing.T) {
	oldCursor := "provider-cursor-old-123456"
	newCursor := "provider-cursor-new-123456"
	guard, err := newExecutionSecretGuard(&oldCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(secretguard.StreamText)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	firstInput := []byte("prefix " + oldCursor[:len(oldCursor)/2])
	first, err := stream.Transform(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	controlResult, err := guard.SanitizeControlResult(map[string]any{
		"providerResumeCursor": newCursor,
		"output": map[string]any{
			"oldCursorCopy": oldCursor,
			"newCursorCopy": newCursor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if controlResult["providerResumeCursor"] != newCursor {
		t.Fatalf("dedicated control Cursor = %#v, want %q", controlResult["providerResumeCursor"], newCursor)
	}
	encoded := mustJSONForSecretGuardTest(t, controlResult["output"])
	if strings.Contains(encoded, oldCursor) || strings.Contains(encoded, newCursor) ||
		!strings.Contains(encoded, secretguard.RedactionMarker) {
		t.Fatalf("control Result duplicates were not sanitized: %s", encoded)
	}

	second, err := stream.Transform([]byte(oldCursor[len(oldCursor)/2:] + " suffix"))
	if err != nil {
		t.Fatalf("adding the control Cursor closed an active stream: %v", err)
	}
	final, err := stream.Finish()
	if err != nil {
		t.Fatal(err)
	}
	streamOutput := append(append(first, second...), final...)
	if strings.Contains(string(streamOutput), oldCursor) ||
		!strings.Contains(string(streamOutput), secretguard.RedactionMarker) {
		t.Fatalf("active stream output was not sanitized: %q", streamOutput)
	}
	latest, err := guard.SanitizeMap(map[string]any{"cursor": newCursor})
	if err != nil || latest["cursor"] != secretguard.RedactionMarker {
		t.Fatalf("new Cursor was not registered for subsequent sanitization: %#v, %v", latest, err)
	}
}

func TestExecutionSecretGuardRejectsSecretInEventType(t *testing.T) {
	secret := "event-type-secret-123456"
	guard := executionGuardForSecretTest(t, secret)
	_, err := guard.SanitizeRunnerMessage(RunnerMessage{
		Type: "event", EventType: "runtime." + secret, Payload: map[string]any{},
	})
	if !secretguard.IsExposure(err) || runnerFailureCode(err) != secretguard.ErrorCode {
		t.Fatalf("unsafe EventType error = %T %v", err, err)
	}
}

func TestExecutionSecretGuardProviderCredentialFieldValidation(t *testing.T) {
	guard, err := newExecutionSecretGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()

	ordinary := "https://provider.example.test/ordinary-setting"
	if err := guard.AddProviderCredential(&RunnerCredential{Payload: map[string]any{
		"baseUrl": ordinary, "organization": "ordinary-organization",
	}}); err != nil {
		t.Fatalf("unknown Provider fields should not be registered: %v", err)
	}
	sanitized, err := guard.SanitizeMap(map[string]any{"baseUrl": ordinary})
	if err != nil || sanitized["baseUrl"] != ordinary {
		t.Fatalf("ordinary Provider field changed: %#v, %v", sanitized, err)
	}

	err = guard.AddProviderCredential(&RunnerCredential{Payload: map[string]any{"apiKey": 42}})
	var exposure *secretguard.ExposureError
	if !errors.As(err, &exposure) || exposure.Code != secretguard.ErrorCode {
		t.Fatalf("invalid explicit credential field error = %T %v", err, err)
	}
}

func mustJSONForSecretGuardTest(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
