package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ExportResult struct {
	Request Request      `json:"request"`
	Bundle  ExportBundle `json:"bundle"`
}

type ExportBundle struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	PrivacyRequestID uuid.UUID              `json:"privacyRequestId"`
	TenantID         uuid.UUID              `json:"tenantId"`
	Subject          exportSubject          `json:"subject"`
	Membership       exportMembership       `json:"membership"`
	Organizations    []exportOrganization   `json:"organizations"`
	Projects         []exportProject        `json:"projects"`
	Sessions         []exportSession        `json:"sessions"`
	Turns            []exportTurn           `json:"turns"`
	Artifacts        []exportArtifact       `json:"artifacts"`
	Credentials      []exportCredential     `json:"credentials"`
	AuditEvents      []exportAuditEvent     `json:"auditEvents"`
	Requests         []exportPrivacyRequest `json:"privacyRequests"`
	GeneratedAt      time.Time              `json:"generatedAt"`
}

type exportSubject struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	DeletedAt   *time.Time `json:"deletedAt"`
}

type exportMembership struct {
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	JoinedAt  *time.Time `json:"joinedAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type exportOrganization struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type exportProject struct {
	ID             uuid.UUID `json:"id"`
	OrganizationID uuid.UUID `json:"organizationId"`
	Name           string    `json:"name"`
	Visibility     string    `json:"visibility"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type exportSession struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	ProjectID      uuid.UUID  `json:"projectId"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Visibility     string     `json:"visibility"`
	Provider       string     `json:"provider"`
	Model          *string    `json:"model"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ArchivedAt     *time.Time `json:"archivedAt"`
}

type exportTurn struct {
	ID              uuid.UUID  `json:"id"`
	SessionID       uuid.UUID  `json:"sessionId"`
	Status          string     `json:"status"`
	InputText       string     `json:"inputText"`
	TurnKind        string     `json:"turnKind"`
	RuntimeMode     string     `json:"runtimeMode"`
	InteractionMode string     `json:"interactionMode"`
	CreatedAt       time.Time  `json:"createdAt"`
	CompletedAt     *time.Time `json:"completedAt"`
}

type exportArtifact struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	ProjectID      uuid.UUID  `json:"projectId"`
	SessionID      uuid.UUID  `json:"sessionId"`
	ExecutionID    *uuid.UUID `json:"executionId"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	OriginalName   *string    `json:"originalName"`
	ContentType    *string    `json:"contentType"`
	SizeBytes      *int64     `json:"sizeBytes"`
	SHA256         *string    `json:"sha256"`
	CreatedAt      time.Time  `json:"createdAt"`
	DeletedAt      *time.Time `json:"deletedAt"`
}

type exportCredential struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID *uuid.UUID `json:"organizationId"`
	Name           string     `json:"name"`
	Purpose        string     `json:"purpose"`
	Provider       string     `json:"provider"`
	CredentialType string     `json:"credentialType"`
	Scope          string     `json:"scope"`
	Version        int        `json:"version"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	RevokedAt      *time.Time `json:"revokedAt"`
}

type exportAuditEvent struct {
	EventID        uuid.UUID      `json:"eventId"`
	Action         string         `json:"action"`
	ResourceType   string         `json:"resourceType"`
	ResourceID     *uuid.UUID     `json:"resourceId"`
	OrganizationID *uuid.UUID     `json:"organizationId"`
	RequestID      string         `json:"requestId"`
	Metadata       map[string]any `json:"metadata"`
	OccurredAt     time.Time      `json:"occurredAt"`
}

type exportPrivacyRequest struct {
	ID           uuid.UUID  `json:"id"`
	RequestType  string     `json:"requestType"`
	Status       string     `json:"status"`
	Version      int64      `json:"version"`
	IntakeReason string     `json:"intakeReason"`
	DueAt        time.Time  `json:"dueAt"`
	CompletedAt  *time.Time `json:"completedAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

func (s *Service) ExecuteExport(
	ctx context.Context,
	principal identity.Principal,
	tenantID, privacyRequestID uuid.UUID,
	expectedVersion int64,
	httpRequestID, ipAddress string,
) (ExportResult, error) {
	model, _, err := s.authorizedRequest(ctx, principal, tenantID, privacyRequestID)
	if err != nil {
		return ExportResult{}, err
	}
	if model.RequestType != "access_export" {
		return ExportResult{}, problem.New(409, "privacy_request_type_mismatch", "Only an access_export Privacy Request can produce an export.")
	}
	if model.Status != "approved" || model.Version != expectedVersion {
		return ExportResult{}, problem.New(409, "privacy_request_version_conflict", "Privacy Request must be approved at the expected version.")
	}
	processing, err := s.transition(ctx, tenantID, privacyRequestID, expectedVersion, "approved", "processing",
		principal.UserID, "Generate the approved Tenant-scoped personal data export.", map[string]any{}, nil,
		httpRequestID, ipAddress)
	if err != nil {
		return ExportResult{}, err
	}
	bundle, buildErr := s.buildExportBundle(ctx, processing)
	if buildErr != nil {
		s.failProcessing(ctx, processing, principal.UserID, "Personal data export generation failed and requires an administrator retry.", httpRequestID, ipAddress)
		return ExportResult{}, buildErr
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		s.failProcessing(ctx, processing, principal.UserID, "Personal data export serialization failed and requires an administrator retry.", httpRequestID, ipAddress)
		return ExportResult{}, problem.Wrap(500, "privacy_export_encode_failed", "Personal data export could not be encoded.", err)
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	summary := map[string]any{
		"schemaVersion": bundle.SchemaVersion, "bytes": len(encoded),
		"sessions": len(bundle.Sessions), "turns": len(bundle.Turns), "artifacts": len(bundle.Artifacts),
	}
	completed, err := s.transition(ctx, tenantID, privacyRequestID, processing.Version, "processing", "completed",
		principal.UserID, "The approved Tenant-scoped personal data export completed.", summary, &digest,
		httpRequestID, ipAddress)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Request: toRequest(completed), Bundle: bundle}, nil
}

func (s *Service) buildExportBundle(
	ctx context.Context,
	request persistence.PrivacyRequest,
) (ExportBundle, error) {
	bundle := ExportBundle{
		SchemaVersion: "synara-privacy-export-v1", PrivacyRequestID: request.ID,
		TenantID: request.TenantID, GeneratedAt: s.now(),
		Organizations: []exportOrganization{}, Projects: []exportProject{}, Sessions: []exportSession{},
		Turns: []exportTurn{}, Artifacts: []exportArtifact{}, Credentials: []exportCredential{},
		AuditEvents: []exportAuditEvent{}, Requests: []exportPrivacyRequest{},
	}
	queries := []struct {
		code    string
		message string
		query   *gorm.DB
	}{
		{"privacy_export_subject_load_failed", "Personal data export could not load the subject.", s.db.WithContext(ctx).Table("users").Select("id, email, display_name, status, created_at, deleted_at").Where("id = ?", request.SubjectUserID).Take(&bundle.Subject)},
		{"privacy_export_membership_load_failed", "Personal data export could not load the Tenant membership.", s.db.WithContext(ctx).Table("tenant_memberships").Select("role, status, joined_at, created_at, updated_at").Where("tenant_id = ? AND user_id = ?", request.TenantID, request.SubjectUserID).Take(&bundle.Membership)},
		{"privacy_export_organizations_load_failed", "Personal data export could not load Organization memberships.", s.db.WithContext(ctx).Table("organization_memberships AS membership").Select("organization.id, organization.name, membership.role, membership.status, membership.created_at, membership.updated_at").Joins("JOIN organizations AS organization ON organization.tenant_id = membership.tenant_id AND organization.id = membership.organization_id").Where("membership.tenant_id = ? AND membership.user_id = ?", request.TenantID, request.SubjectUserID).Order("organization.id").Scan(&bundle.Organizations)},
		{"privacy_export_projects_load_failed", "Personal data export could not load Projects.", s.db.WithContext(ctx).Table("projects").Select("id, organization_id, name, visibility, created_at, updated_at").Where("tenant_id = ? AND created_by = ?", request.TenantID, request.SubjectUserID).Order("created_at, id").Scan(&bundle.Projects)},
		{"privacy_export_sessions_load_failed", "Personal data export could not load Sessions.", s.db.WithContext(ctx).Table("agent_sessions").Select("id, organization_id, project_id, title, status, visibility, provider, model, created_at, updated_at, archived_at").Where("tenant_id = ? AND created_by = ?", request.TenantID, request.SubjectUserID).Order("created_at, id").Scan(&bundle.Sessions)},
		{"privacy_export_turns_load_failed", "Personal data export could not load Turns.", s.db.WithContext(ctx).Table("agent_turns").Select("id, session_id, status, input_text, turn_kind, runtime_mode, interaction_mode, created_at, completed_at").Where("tenant_id = ? AND created_by = ?", request.TenantID, request.SubjectUserID).Order("created_at, id").Scan(&bundle.Turns)},
		{"privacy_export_artifacts_load_failed", "Personal data export could not load Artifacts.", s.db.WithContext(ctx).Table("artifacts").Select("id, organization_id, project_id, session_id, execution_id, kind, status, original_name, content_type, size_bytes, sha256, created_at, deleted_at").Where("tenant_id = ? AND created_by_type = ? AND created_by_id = ?", request.TenantID, "user", request.SubjectUserID).Order("created_at, id").Scan(&bundle.Artifacts)},
		{"privacy_export_credentials_load_failed", "Personal data export could not load Credential metadata.", s.db.WithContext(ctx).Table("provider_credentials").Select("id, organization_id, name, purpose, provider, credential_type, scope, version, created_at, updated_at, expires_at, revoked_at").Where("tenant_id = ? AND scope_user_id = ?", request.TenantID, request.SubjectUserID).Order("created_at, id").Scan(&bundle.Credentials)},
		{"privacy_export_requests_load_failed", "Personal data export could not load Privacy Requests.", s.db.WithContext(ctx).Table("privacy_requests").Select("id, request_type, status, version, intake_reason, due_at, completed_at, created_at, updated_at").Where("tenant_id = ? AND subject_user_id = ?", request.TenantID, request.SubjectUserID).Order("created_at, id").Scan(&bundle.Requests)},
	}
	for _, query := range queries {
		if err := query.query.Error; err != nil {
			return ExportBundle{}, problem.Wrap(500, query.code, query.message, err)
		}
	}
	var auditModels []persistence.AuditLog
	if err := s.db.WithContext(ctx).
		Select("event_id", "action", "resource_type", "resource_id", "organization_id", "request_id", "metadata", "occurred_at").
		Where("tenant_id = ? AND actor_id = ?", request.TenantID, request.SubjectUserID).
		Order("occurred_at, event_id").Find(&auditModels).Error; err != nil {
		return ExportBundle{}, problem.Wrap(500, "privacy_export_audit_load_failed", "Personal data export could not load Audit events.", err)
	}
	for _, model := range auditModels {
		bundle.AuditEvents = append(bundle.AuditEvents, exportAuditEvent{
			EventID: model.EventID, Action: model.Action, ResourceType: model.ResourceType,
			ResourceID: model.ResourceID, OrganizationID: model.OrganizationID,
			RequestID: model.RequestID, Metadata: model.Metadata, OccurredAt: model.OccurredAt,
		})
	}
	return bundle, nil
}

func (s *Service) failProcessing(
	ctx context.Context,
	request persistence.PrivacyRequest,
	actorUserID uuid.UUID,
	reason, httpRequestID, ipAddress string,
) {
	_, _ = s.transition(ctx, request.TenantID, request.ID, request.Version, "processing", "failed",
		actorUserID, reason, map[string]any{"retryable": true}, nil, httpRequestID, ipAddress)
}
