package httpapi

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func (s *Server) createArtifact(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input artifacts.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	grant, err := s.artifacts.Create(r.Context(), mustPrincipal(r), sessionID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	items, err := s.artifacts.List(r.Context(), mustPrincipal(r), sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	item, err := s.artifacts.Get(r.Context(), mustPrincipal(r), artifactID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) completeArtifact(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	var input artifacts.CompleteInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.artifacts.Complete(r.Context(), mustPrincipal(r), artifactID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	grant, err := s.artifacts.Download(r.Context(), mustPrincipal(r), artifactID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	if err := s.artifacts.Delete(r.Context(), mustPrincipal(r), artifactID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createWorkerArtifact(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input artifacts.WorkerCreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	if idempotencyKey := r.Header.Get(artifacts.WorkerIdempotencyKeyHeader); idempotencyKey != "" {
		input.IdempotencyKey = &idempotencyKey
	}
	grant, err := s.artifacts.CreateForWorker(r.Context(), mustWorker(r), executionID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (s *Server) completeWorkerArtifact(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	var input artifacts.WorkerCompleteInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.artifacts.CompleteForWorker(r.Context(), mustWorker(r), executionID, artifactID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) downloadWorkerCheckpointArtifact(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	checkpointID, ok := s.pathUUID(w, r, "checkpointID")
	if !ok {
		return
	}
	var input executions.LeaseInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	grant, err := s.artifacts.DownloadCheckpointForWorker(
		r.Context(), mustWorker(r), executionID, checkpointID, input,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

func (s *Server) uploadArtifactContent(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	if err := s.artifacts.UploadLocal(
		r.Context(), artifactID, r.URL.Query().Get("token"), r.Header.Get("Content-Type"), r.ContentLength, r.Body,
	); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) downloadArtifactContent(w http.ResponseWriter, r *http.Request) {
	artifactID, ok := s.pathUUID(w, r, "artifactID")
	if !ok {
		return
	}
	item, reader, err := s.artifacts.OpenDownload(r.Context(), artifactID, r.URL.Query().Get("token"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer reader.Close()
	if item.ContentType != nil {
		w.Header().Set("Content-Type", *item.ContentType)
	}
	if item.SizeBytes != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*item.SizeBytes, 10))
	}
	if item.OriginalName != nil {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": *item.OriginalName}))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, reader); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		s.logger.Warn("artifact download stream failed", "artifactId", artifactID, "error", err)
	}
}
