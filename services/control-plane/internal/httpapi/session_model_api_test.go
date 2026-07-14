package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestSessionModelSwitchRoutePersistsAndReplaysExactlyOnce(t *testing.T) {
	fixture := newProviderCapabilityHTTPFixture(t)
	completeProviderCapabilityHTTPExecution(t, fixture)
	seedProviderCapabilityHTTPWorker(t, fixture)
	path := "/v1/sessions/" + fixture.sessionID.String() + "/model-switch"
	body := `{"model":"gpt-5.6","expectedModel":null}`

	first := fixture.modelSwitchRequest(t, http.MethodPost, fixture.memberToken, path, body, "http-model-switch")
	if first.Code != http.StatusOK {
		t.Fatalf("first model switch status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstSession sessions.Session
	if err := json.Unmarshal(first.Body.Bytes(), &firstSession); err != nil {
		t.Fatal(err)
	}
	if firstSession.Model == nil || *firstSession.Model != "gpt-5.6" {
		t.Fatalf("first model switch response = %#v", firstSession)
	}
	if first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("first model switch was marked replayed: %#v", first.Header())
	}

	replay := fixture.modelSwitchRequest(t, http.MethodPost, fixture.memberToken, path, body, "http-model-switch")
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("model switch replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	var replayedSession sessions.Session
	if err := json.Unmarshal(replay.Body.Bytes(), &replayedSession); err != nil {
		t.Fatal(err)
	}
	if replayedSession.ID != firstSession.ID || replayedSession.LastEventSequence != firstSession.LastEventSequence {
		t.Fatalf("replayed Session mismatch: first=%#v replay=%#v", firstSession, replayedSession)
	}

	var eventCount, auditCount int64
	if err := fixture.db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, fixture.sessionID, "session.model.changed").
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", fixture.tenantID, fixture.sessionID, "session.model.changed").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || auditCount != 1 {
		t.Fatalf("model switch event/audit counts = %d/%d", eventCount, auditCount)
	}
	bindings := make([]persistence.ProviderRuntimeBinding, 0)
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Order("revision").Find(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0].Status != "released" || bindings[1].Status != "active" ||
		bindings[1].Revision != bindings[0].Revision+1 {
		t.Fatalf("HTTP model switch bindings = %#v", bindings)
	}
	var stored persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CurrentRuntimeBindingID == nil || *stored.CurrentRuntimeBindingID != bindings[1].ID ||
		stored.LastEventSequence != firstSession.LastEventSequence {
		t.Fatalf("stored Session after HTTP model switch = %#v", stored)
	}

	patchResponse := fixture.modelSwitchRequest(t, http.MethodPatch, fixture.memberToken, path, body, "http-model-switch-patch")
	if patchResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH alias unexpectedly exists: status=%d body=%s", patchResponse.Code, patchResponse.Body.String())
	}
}

func TestSessionModelSwitchRouteValidatesCASAndAuthorization(t *testing.T) {
	t.Run("active execution", func(t *testing.T) {
		fixture := newProviderCapabilityHTTPFixture(t)
		path := "/v1/sessions/" + fixture.sessionID.String() + "/model-switch"
		response := fixture.modelSwitchRequest(
			t, http.MethodPost, fixture.memberToken, path,
			`{"model":"gpt-5.6","expectedModel":null}`, "http-model-switch-active",
		)
		assertProblemResponse(t, response, http.StatusConflict, "session_execution_active")
	})

	t.Run("request contract", func(t *testing.T) {
		fixture := newProviderCapabilityHTTPFixture(t)
		completeProviderCapabilityHTTPExecution(t, fixture)
		seedProviderCapabilityHTTPWorker(t, fixture)
		path := "/v1/sessions/" + fixture.sessionID.String() + "/model-switch"
		for _, test := range []struct {
			name string
			body string
			code string
		}{
			{name: "expected omitted", body: `{"model":"gpt-5.6"}`, code: "expected_model_required"},
			{name: "expected wrong type", body: `{"model":"gpt-5.6","expectedModel":42}`, code: "invalid_expected_model"},
			{name: "empty model", body: `{"model":" ","expectedModel":null}`, code: "invalid_model"},
			{name: "stale expected", body: `{"model":"gpt-5.6","expectedModel":"gpt-4"}`, code: "session_model_conflict"},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := fixture.modelSwitchRequest(
					t, http.MethodPost, fixture.memberToken, path, test.body, "http-contract-"+strings.ReplaceAll(test.name, " ", "-"),
				)
				status := http.StatusBadRequest
				if test.code == "session_model_conflict" {
					status = http.StatusConflict
				}
				assertProblemResponse(t, response, status, test.code)
			})
		}
	})

	t.Run("private isolation", func(t *testing.T) {
		fixture := newProviderCapabilityHTTPFixture(t)
		path := "/v1/sessions/" + fixture.privateSessionID.String() + "/model-switch"
		response := fixture.modelSwitchRequest(
			t, http.MethodPost, fixture.memberToken, path,
			`{"model":"gpt-5.6","expectedModel":null}`, "http-private-model-switch",
		)
		assertProblemResponse(t, response, http.StatusNotFound, "session_not_found")
	})

	t.Run("authentication", func(t *testing.T) {
		fixture := newProviderCapabilityHTTPFixture(t)
		path := "/v1/sessions/" + fixture.sessionID.String() + "/model-switch"
		response := fixture.modelSwitchRequest(
			t, http.MethodPost, "", path,
			`{"model":"gpt-5.6","expectedModel":null}`, "http-unauthenticated-model-switch",
		)
		assertProblemResponse(t, response, http.StatusUnauthorized, "authentication_required")
	})
}

func completeProviderCapabilityHTTPExecution(t *testing.T, fixture providerCapabilityHTTPFixture) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.executionID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.ID).
			Updates(map[string]any{"status": "completed", "finished_at": now, "worker_id": nil}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentTurn{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, execution.TurnID).
			Updates(map[string]any{"status": "completed", "completed_at": now}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func seedProviderCapabilityHTTPWorker(t *testing.T, fixture providerCapabilityHTTPFixture) {
	t.Helper()
	manifestID := uuid.New()
	seedProviderCapabilityManifest(t, fixture.db, manifestID, "native")
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: fixture.targetID,
		TargetKind: "kubernetes", ClusterID: "model-switch-cluster", Namespace: "model-switch",
		PodName: uuid.NewString(), Version: "model-switch-worker", ProtocolVersion: 2,
		Capabilities: map[string]any{}, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func (fixture providerCapabilityHTTPFixture) modelSwitchRequest(
	t *testing.T,
	method, token, path, body, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
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
