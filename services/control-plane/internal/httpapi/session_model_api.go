package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

type switchSessionModelRequest struct {
	Model         string          `json:"model"`
	ExpectedModel json.RawMessage `json:"expectedModel"`
}

func (s *Server) switchSessionModel(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var request switchSessionModelRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	input, err := request.input()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	item, replayed, err := s.sessions.SwitchModelWithIdempotency(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, item)
}

func (request switchSessionModelRequest) input() (sessions.SwitchModelInput, error) {
	if request.ExpectedModel == nil {
		return sessions.SwitchModelInput{}, problem.New(
			400, "expected_model_required", "expectedModel must be provided and may be null.",
		)
	}
	expectedJSON := bytes.TrimSpace(request.ExpectedModel)
	if bytes.Equal(expectedJSON, []byte("null")) {
		return sessions.SwitchModelInput{
			Model: request.Model, ExpectedModelProvided: true,
		}, nil
	}
	var expectedModel string
	if err := json.Unmarshal(expectedJSON, &expectedModel); err != nil {
		return sessions.SwitchModelInput{}, problem.New(
			400, "invalid_expected_model", "expectedModel must be null or a valid model name.",
		)
	}
	return sessions.SwitchModelInput{
		Model: request.Model, ExpectedModel: &expectedModel, ExpectedModelProvided: true,
	}, nil
}
