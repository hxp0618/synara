package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
)

func (s *Server) getPlatformProfile(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.config.Platform.Public())
}

func (s *Server) listExecutionTargets(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.targets.List(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createExecutionTarget(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input executiontargets.CreateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.targets.Create(r.Context(), mustPrincipal(r), tenantID, input)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getExecutionTarget(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	item, err := s.targets.Get(r.Context(), mustPrincipal(r), tenantID, targetID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) installSSHExecutionTarget(w http.ResponseWriter, r *http.Request) {
	s.provisionSSHExecutionTarget(w, r, "install")
}

func (s *Server) upgradeSSHExecutionTarget(w http.ResponseWriter, r *http.Request) {
	s.provisionSSHExecutionTarget(w, r, "upgrade")
}

func (s *Server) revokeSSHExecutionTarget(w http.ResponseWriter, r *http.Request) {
	s.provisionSSHExecutionTarget(w, r, "revoke")
}

func (s *Server) provisionSSHExecutionTarget(w http.ResponseWriter, r *http.Request, operation string) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var (
		result executiontargets.SSHProvisionResult
		err    error
	)
	switch operation {
	case "install":
		result, err = s.sshTargets.Install(r.Context(), mustPrincipal(r), tenantID, targetID, requestID(r), clientIP(r))
	case "upgrade":
		result, err = s.sshTargets.Upgrade(r.Context(), mustPrincipal(r), tenantID, targetID, requestID(r), clientIP(r))
	case "revoke":
		result, err = s.sshTargets.Revoke(r.Context(), mustPrincipal(r), tenantID, targetID, requestID(r), clientIP(r))
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
