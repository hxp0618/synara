package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) registerWorker(w http.ResponseWriter, r *http.Request) {
	configured := strings.TrimSpace(s.config.WorkerRegistrationToken)
	provided := bearerToken(r)
	if configured == "" {
		s.writeError(w, r, problem.New(503, "worker_registration_disabled", "Worker registration is not configured."))
		return
	}
	if len(provided) != len(configured) || subtle.ConstantTimeCompare([]byte(provided), []byte(configured)) != 1 {
		s.writeError(w, r, problem.New(401, "invalid_worker_registration_token", "The worker registration token is invalid."))
		return
	}
	var input executions.RegisterWorkerInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	registered, err := s.executions.Register(r.Context(), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, registered)
}

func (s *Server) workerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var input executions.HeartbeatInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	worker, err := s.executions.Heartbeat(r.Context(), mustWorker(r), input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (s *Server) claimExecution(w http.ResponseWriter, r *http.Request) {
	var input executions.ClaimExecutionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Claim(r.Context(), mustWorker(r), input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) renewExecutionLease(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.RenewLeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Renew(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) startExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.LeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Start(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) completeExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.CompleteExecutionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Complete(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) failExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.FailExecutionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Fail(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) releaseExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.ReleaseLeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.Release(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) appendRuntimeEvent(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.RuntimeEventInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.AppendRuntimeEvent(r.Context(), mustWorker(r), executionID, input, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOperation(w, result.Replayed, result.StatusCode, result.Value)
}

func (s *Server) requireWorker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		worker, err := s.executions.Authenticate(r.Context(), bearerToken(r))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), workerContextKey{}, worker)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	prefix, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func mustWorker(r *http.Request) persistence.WorkerInstance {
	return r.Context().Value(workerContextKey{}).(persistence.WorkerInstance)
}

func writeOperation(w http.ResponseWriter, replayed bool, status int, body any) {
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, status, body)
}
