package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestSessionSettlementRoutePersistsAndReplaysExactlyOnce(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	path := "/v1/sessions/" + fixture.sessionID.String() + "/settled"
	body := `{"settled":true}`

	first := fixture.modelSwitchRequest(
		t, http.MethodPut, fixture.memberToken, path, body, "http-session-settle",
	)
	if first.Code != http.StatusOK {
		t.Fatalf("first settle status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstSession sessions.Session
	if err := json.Unmarshal(first.Body.Bytes(), &firstSession); err != nil {
		t.Fatal(err)
	}
	if firstSession.SettledAt == nil {
		t.Fatalf("first settle response = %#v", firstSession)
	}

	replay := fixture.modelSwitchRequest(
		t, http.MethodPut, fixture.memberToken, path, body, "http-session-settle",
	)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("settle replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}

	unsettled := fixture.modelSwitchRequest(
		t, http.MethodPut, fixture.memberToken, path, `{"settled":false}`, "http-session-unsettle",
	)
	if unsettled.Code != http.StatusOK {
		t.Fatalf("unsettle status = %d, body = %s", unsettled.Code, unsettled.Body.String())
	}
	var unsettledSession sessions.Session
	if err := json.Unmarshal(unsettled.Body.Bytes(), &unsettledSession); err != nil {
		t.Fatal(err)
	}
	if unsettledSession.SettledAt != nil {
		t.Fatalf("unsettle response = %#v", unsettledSession)
	}

	postAlias := fixture.modelSwitchRequest(
		t, http.MethodPost, fixture.memberToken, path, body, "http-session-settle-post",
	)
	if postAlias.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST alias unexpectedly exists: status=%d body=%s", postAlias.Code, postAlias.Body.String())
	}
}
