package httpapi

import (
	"net/http"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
)

func (s *Server) getPlatformProfile(w http.ResponseWriter, _ *http.Request) {
	profile := s.config.Platform.Public()
	resourceLifecyclePolicy := s.config.ResourceLifecycle
	if s.lifecyclePolicies != nil {
		resourceLifecyclePolicy = s.lifecyclePolicies.PublicConfig()
	} else if resourceLifecyclePolicy.Validate() != nil {
		resourceLifecyclePolicy = lifecyclepolicy.DefaultConfig(profile.Profile)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile": profile.Profile, "metadataStore": profile.MetadataStore,
		"artifactStore": profile.ArtifactStore, "queueDriver": profile.QueueDriver,
		"controlPlaneReplicas": profile.ControlPlaneReplicas, "highAvailability": profile.HighAvailability,
		"leaseEnabled": profile.LeaseEnabled, "fencingEnabled": profile.FencingEnabled,
		"executionTargetKinds":     profile.ExecutionTargetKinds,
		"artifactPayloadMigration": profile.ArtifactPayloadMigration,
		"metadataExportImport":     profile.MetadataExportImport,
		"resourceLifecyclePolicy":  resourceLifecyclePolicy,
	})
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

func (s *Server) updateExecutionTargetProviderPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var policy map[string]any
	if err := decodeJSON(r, &policy); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.targets.UpdateProviderPolicy(r.Context(), mustPrincipal(r), tenantID, targetID, policy)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateExecutionTargetProcessContainmentPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	var policy map[string]any
	if err := decodeJSON(r, &policy); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.targets.UpdateProcessContainmentPolicy(r.Context(), mustPrincipal(r), tenantID, targetID, policy)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) disableManagedKubernetesExecutionTarget(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	targetID, ok := s.pathUUID(w, r, "executionTargetID")
	if !ok {
		return
	}
	item, replayed, err := s.targets.DisableManagedKubernetesTarget(
		r.Context(), mustPrincipal(r), tenantID, targetID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
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
