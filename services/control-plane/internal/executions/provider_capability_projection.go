package executions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var projectedActiveExecutionStatuses = []string{"queued", "leased", "running", "waiting-for-approval", "recovering"}

func (s *Service) ProjectProviderCapabilitiesForProject(
	ctx context.Context,
	principal identity.Principal,
	projectID uuid.UUID,
	requestedTargetID *uuid.UUID,
) (providercapabilities.Projection, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	if s.projects == nil {
		return providercapabilities.Projection{}, problem.New(500, "provider_capabilities_unavailable", "Provider capability projection is not configured.")
	}
	project, err := s.projects.Get(ctx, principal, tenantID, projectID)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	binding, err := s.targets.ResolveForSession(ctx, tenantID, project.OrganizationID, requestedTargetID)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	var target persistence.ExecutionTarget
	err = s.db.WithContext(ctx).
		Where("id = ? AND status = ?", binding.ID, "active").
		Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, project.OrganizationID).
		Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return providercapabilities.Projection{}, problem.New(409, "execution_target_unavailable", "The selected Execution Target is no longer available.")
	}
	if err != nil {
		return providercapabilities.Projection{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the selected Execution Target.", err)
	}
	projection, err := providercapabilities.LoadTargetProjection(
		ctx, s.db, target, s.now(), s.heartbeatTimeout,
	)
	if err != nil {
		return providercapabilities.Projection{}, capabilityProjectionError(err)
	}
	return projection, nil
}

func (s *Service) ProjectProviderCapabilitiesForSession(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (providercapabilities.Projection, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	session, err := s.sessions.Get(ctx, principal, tenantID, sessionID)
	if err != nil {
		return providercapabilities.Projection{}, err
	}

	var execution persistence.AgentExecution
	executionErr := s.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND status IN ?", tenantID, sessionID, projectedActiveExecutionStatuses).
		Take(&execution).Error
	if executionErr != nil && !errors.Is(executionErr, gorm.ErrRecordNotFound) {
		return providercapabilities.Projection{}, problem.Wrap(500, "execution_lookup_failed", "Failed to load the active Execution.", executionErr)
	}
	if executionErr == nil {
		var target persistence.ExecutionTarget
		if err := s.db.WithContext(ctx).Where("id = ?", execution.ExecutionTargetID).Take(&target).Error; err != nil {
			return providercapabilities.Projection{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the active Execution Target.", err)
		}
		provider := session.Provider
		if execution.Provider != nil && *execution.Provider != "" {
			provider = *execution.Provider
		}
		projection, err := providercapabilities.LoadExecutionProjection(ctx, s.db, target, execution, provider)
		if err != nil {
			return providercapabilities.Projection{}, capabilityProjectionError(err)
		}
		if err := s.applySessionCapabilityConstraints(
			ctx, s.db, tenantID, sessionID, provider, &projection,
		); err != nil {
			return providercapabilities.Projection{}, err
		}
		return projection, nil
	}

	var target persistence.ExecutionTarget
	targetErr := s.db.WithContext(ctx).
		Where("id = ?", session.ExecutionTargetID).
		Where("(tenant_id IS NULL OR tenant_id = ?) AND (organization_id IS NULL OR organization_id = ?)", tenantID, session.OrganizationID).
		Take(&target).Error
	if targetErr != nil {
		return providercapabilities.Projection{}, problem.Wrap(500, "execution_target_lookup_failed", "Failed to load the Session Execution Target.", targetErr)
	}
	projection, err := s.projectIdleSessionProviderCapabilities(ctx, s.db, target, session)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	if err := s.applySessionCapabilityConstraints(
		ctx, s.db, tenantID, sessionID, session.Provider, &projection,
	); err != nil {
		return providercapabilities.Projection{}, err
	}
	return projection, nil
}

func (s *Service) projectIdleSessionProviderCapabilities(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	session sessions.Session,
) (providercapabilities.Projection, error) {
	projection, err := providercapabilities.LoadTargetProjection(
		ctx, db, target, s.now(), s.heartbeatTimeout,
	)
	if err != nil {
		return providercapabilities.Projection{}, capabilityProjectionError(err)
	}
	filtered := projection.Items[:0]
	for _, item := range projection.Items {
		if canonicalProviderName(item.Provider) == canonicalProviderName(session.Provider) {
			filtered = append(filtered, item)
		}
	}
	projection.Items = filtered
	if !projectionHasUnobservedCapabilities(projection) {
		return projection, nil
	}

	bound, found, err := s.loadBoundSessionProviderCapabilities(ctx, db, target, session)
	if err != nil {
		return providercapabilities.Projection{}, err
	}
	if !found {
		return projection, nil
	}
	boundByCapability := make(map[string]providercapabilities.Item, len(bound.Items))
	for _, item := range bound.Items {
		boundByCapability[item.CapabilityID] = item
	}
	replaced := false
	for index := range projection.Items {
		item := &projection.Items[index]
		if item.Status != providercapabilities.StatusUnobserved {
			continue
		}
		observed, exists := boundByCapability[item.CapabilityID]
		if !exists || observed.Status != providercapabilities.StatusSupported {
			continue
		}
		*item = observed
		replaced = true
	}
	if replaced {
		projection.Basis = providercapabilities.BasisExecution
		projection.ExecutionID = bound.ExecutionID
	}
	return projection, nil
}

func (s *Service) loadBoundSessionProviderCapabilities(
	ctx context.Context,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	session sessions.Session,
) (providercapabilities.Projection, bool, error) {
	binding, found, err := s.sessions.LoadActiveRuntimeBinding(
		ctx, db, session.TenantID, session.ID, session.Provider,
	)
	if err != nil {
		return providercapabilities.Projection{}, false, err
	}
	if !found {
		return providercapabilities.Projection{}, false, nil
	}
	if binding.WorkerManifestID == nil || binding.LastExecutionID == nil {
		return providercapabilities.Projection{}, false, nil
	}
	var execution persistence.AgentExecution
	err = db.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND session_id = ? AND execution_target_id = ? AND provider_runtime_binding_id = ? AND worker_manifest_id = ?",
			session.TenantID, *binding.LastExecutionID, session.ID, target.ID,
			binding.ID, *binding.WorkerManifestID).
		Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return providercapabilities.Projection{}, false, nil
	}
	if err != nil {
		return providercapabilities.Projection{}, false,
			problem.Wrap(500, "runtime_binding_execution_load_failed", "The Session Provider runtime execution could not be loaded.", err)
	}
	projection, err := providercapabilities.LoadExecutionProjection(
		ctx, db, target, execution, session.Provider,
	)
	if err != nil {
		return providercapabilities.Projection{}, false, capabilityProjectionError(err)
	}
	return projection, true, nil
}

func projectionHasUnobservedCapabilities(projection providercapabilities.Projection) bool {
	for _, item := range projection.Items {
		if item.Status == providercapabilities.StatusUnobserved {
			return true
		}
	}
	return false
}

func (s *Service) applySessionCapabilityConstraints(
	ctx context.Context,
	db *gorm.DB,
	tenantID, sessionID uuid.UUID,
	provider string,
	projection *providercapabilities.Projection,
) error {
	if canonicalProviderName(provider) != "codex" {
		return nil
	}
	var cursor struct {
		State     string `gorm:"column:provider_resume_cursor_state"`
		Encrypted []byte `gorm:"column:provider_resume_cursor_encrypted"`
	}
	if err := db.WithContext(ctx).Table("agent_sessions").
		Select("provider_resume_cursor_state", "provider_resume_cursor_encrypted").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).
		Take(&cursor).Error; err != nil {
		return problem.Wrap(500, "session_cursor_state_load_failed", "Failed to load the Session Provider Cursor state.", err)
	}
	if cursor.State == "usable" && len(cursor.Encrypted) > 0 {
		return nil
	}
	for index := range projection.Items {
		item := &projection.Items[index]
		if canonicalProviderName(item.Provider) != "codex" || item.CapabilityID != "compact" {
			continue
		}
		item.Status = providercapabilities.StatusUnsupported
		item.ReasonCode = providercapabilities.ReasonProviderCursorRequired
		item.SupportMode = nil
	}
	return nil
}

func capabilityProjectionError(err error) error {
	if errors.Is(err, providercapabilities.ErrInvalidManifest) {
		return problem.Wrap(500, "worker_manifest_projection_invalid", "A stored Worker manifest is invalid.", err)
	}
	return problem.Wrap(500, "provider_capabilities_load_failed", "Provider capabilities could not be loaded.", err)
}

func canonicalProviderName(value string) string {
	if canonical, valid := executiontargets.CanonicalStage3Provider(value); valid {
		return canonical
	}
	return value
}
