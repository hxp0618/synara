package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestAdvancedSessionOperationRoutesReturnStableStatusAndReplayHeader(t *testing.T) {
	for _, operation := range []string{"compact", "review", "rollback", "fork"} {
		t.Run(operation, func(t *testing.T) {
			fixture, path, body, wantStatus := prepareAdvancedSessionOperationHTTP(t, operation)
			idempotencyKey := "http-" + operation + "-replay"

			first := fixture.sessionOperationRequest(t, fixture.operatorToken, path, body, idempotencyKey)
			if first.Code != wantStatus || first.Header().Get("Idempotency-Replayed") != "" {
				t.Fatalf("first %s status=%d headers=%v body=%s", operation, first.Code, first.Header(), first.Body.String())
			}
			replayed := fixture.sessionOperationRequest(t, fixture.operatorToken, path, body, idempotencyKey)
			if replayed.Code != wantStatus || replayed.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatalf("replayed %s status=%d headers=%v body=%s", operation, replayed.Code, replayed.Header(), replayed.Body.String())
			}
			if first.Body.String() != replayed.Body.String() {
				t.Fatalf("%s replay body changed:\nfirst=%s\nreplay=%s", operation, first.Body.String(), replayed.Body.String())
			}
		})
	}
}

func TestAdvancedSessionOperationRoutesRejectInvalidJSONAndMissingIdempotencyKey(t *testing.T) {
	for _, operation := range []string{"compact", "review", "rollback", "fork"} {
		t.Run(operation, func(t *testing.T) {
			fixture, path, body, _ := prepareAdvancedSessionOperationHTTP(t, operation)
			invalid := fixture.sessionOperationRequest(t, fixture.operatorToken, path, "{", "http-invalid-"+operation)
			assertProblemResponse(t, invalid, http.StatusBadRequest, "invalid_json")

			missingKey := fixture.sessionOperationRequest(t, fixture.operatorToken, path, body, "")
			assertProblemResponse(t, missingKey, http.StatusBadRequest, "idempotency_key_required")
		})
	}
}

func TestAdvancedSessionOperationRoutesDoNotRevealPrivateSessions(t *testing.T) {
	for _, operation := range []string{"compact", "review", "rollback", "fork"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newProviderCapabilityHTTPFixture(t)
			path := "/v1/sessions/" + fixture.privateSessionID.String()
			body := `{"expectedLastEventSequence":0}`
			switch operation {
			case "compact":
				path += "/compact"
			case "review":
				path += "/reviews"
				body = `{"expectedLastEventSequence":0,"target":{"type":"uncommittedChanges"}}`
			case "rollback":
				path += "/rollback"
				body = fmt.Sprintf(`{"expectedLastEventSequence":0,"fromTurnId":%q}`, uuid.NewString())
			case "fork":
				path += "/fork"
			}
			response := fixture.sessionOperationRequest(
				t, fixture.memberToken, path, body, "http-private-"+operation,
			)
			assertProblemResponse(t, response, http.StatusNotFound, "session_not_found")
		})
	}
}

func prepareAdvancedSessionOperationHTTP(
	t *testing.T,
	operation string,
) (providerCapabilityHTTPFixture, string, string, int) {
	t.Helper()
	fixture := newProviderCapabilityHTTPFixture(t)
	completeProviderCapabilityHTTPExecution(t, fixture)
	seedProviderCapabilityHTTPWorker(t, fixture)
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	base := "/v1/sessions/" + fixture.sessionID.String()
	switch operation {
	case "compact":
		if err := fixture.db.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
			Updates(map[string]any{
				"provider_resume_cursor_state":     "usable",
				"provider_resume_cursor_encrypted": []byte("encrypted-http-cursor"),
			}).Error; err != nil {
			t.Fatal(err)
		}
		return fixture, base + "/compact",
			fmt.Sprintf(`{"expectedLastEventSequence":%d}`, session.LastEventSequence), http.StatusAccepted
	case "review":
		return fixture, base + "/reviews",
			fmt.Sprintf(`{"expectedLastEventSequence":%d,"target":{"type":"uncommittedChanges"}}`, session.LastEventSequence),
			http.StatusAccepted
	case "rollback":
		var execution persistence.AgentExecution
		if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.executionID).
			Take(&execution).Error; err != nil {
			t.Fatal(err)
		}
		return fixture, base + "/rollback",
			fmt.Sprintf(`{"expectedLastEventSequence":%d,"fromTurnId":%q}`, session.LastEventSequence, execution.TurnID),
			http.StatusOK
	case "fork":
		return fixture, base + "/fork",
			fmt.Sprintf(`{"expectedLastEventSequence":%d,"title":"HTTP fork"}`, session.LastEventSequence),
			http.StatusCreated
	default:
		t.Fatalf("unsupported advanced Session operation %q", operation)
		return providerCapabilityHTTPFixture{}, "", "", 0
	}
}

func (fixture providerCapabilityHTTPFixture) sessionOperationRequest(
	t *testing.T,
	token, path, body, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: fixture.cookieName, Value: token})
	}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	return response
}
