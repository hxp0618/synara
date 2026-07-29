package agentd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderCredentialBrokerKeepsLongLivedCodexKeyOutsideProvider(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		if request.URL.Path != "/v1/responses" || request.URL.RawQuery != "stream=true" {
			t.Fatalf("brokered upstream URL = %s", request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer long-lived-codex-key" {
			t.Fatalf("brokered upstream Authorization = %q", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(response, "ok")
	}))
	defer upstream.Close()

	credential, broker, err := startProviderCredentialBroker(
		context.Background(),
		"codex",
		&RunnerCredential{Payload: map[string]any{
			"apiKey": "long-lived-codex-key", "baseUrl": upstream.URL + "/v1",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	taskToken, _ := credential.Payload["apiKey"].(string)
	brokerURL, _ := credential.Payload["baseUrl"].(string)
	if taskToken == "" || taskToken == "long-lived-codex-key" || !strings.HasPrefix(taskToken, "synara_task_") {
		t.Fatalf("brokered Provider Credential = %#v", credential.Payload)
	}
	request, _ := http.NewRequest(http.MethodPost, brokerURL+"/responses?stream=true", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer "+taskToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("brokered response = %d, upstream calls = %d", response.StatusCode, upstreamCalls.Load())
	}

	unauthorized, _ := http.NewRequest(http.MethodPost, brokerURL+"/responses", nil)
	unauthorized.Header.Set("Authorization", "Bearer stolen-wrong-token")
	response, err = http.DefaultClient.Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || upstreamCalls.Load() != 1 {
		t.Fatalf("unauthorized broker response = %d, upstream calls = %d", response.StatusCode, upstreamCalls.Load())
	}
}

func TestProviderCredentialBrokerTranslatesClaudeTaskKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" || request.Header.Get("X-Api-Key") != "long-lived-claude-key" ||
			request.Header.Get("Authorization") != "" {
			t.Fatalf("Claude brokered request = %s headers=%#v", request.URL.Path, request.Header)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	credential, broker, err := startProviderCredentialBroker(
		context.Background(),
		"claudeAgent",
		&RunnerCredential{Payload: map[string]any{
			"apiKey": "long-lived-claude-key", "baseUrl": upstream.URL,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	taskToken := credential.Payload["apiKey"].(string)
	request, _ := http.NewRequest(http.MethodPost, credential.Payload["baseUrl"].(string)+"/v1/messages", nil)
	request.Header.Set("X-Api-Key", taskToken)
	request.Header.Set("Authorization", "Bearer attacker-value")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("Claude broker response = %d", response.StatusCode)
	}
}
