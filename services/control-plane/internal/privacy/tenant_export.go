package privacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
)

type TenantExportResult struct {
	Receipt persistence.TenantDataExport `json:"receipt"`
	Bundle  TenantExportBundle           `json:"bundle"`
}

type TenantExportBundle struct {
	SchemaVersion           string                      `json:"schemaVersion"`
	ExportID                uuid.UUID                   `json:"exportId"`
	GeneratedAt             time.Time                   `json:"generatedAt"`
	Tenant                  tenantExportTenant          `json:"tenant"`
	Users                   []tenantExportUser          `json:"users"`
	TenantMemberships       []tenantExportMembership    `json:"tenantMemberships"`
	Organizations           []tenantExportOrganization  `json:"organizations"`
	OrganizationMemberships []tenantExportOrgMembership `json:"organizationMemberships"`
	Projects                []tenantExportProject       `json:"projects"`
	Sessions                []tenantExportSession       `json:"sessions"`
	Turns                   []tenantExportTurn          `json:"turns"`
	SessionEvents           []tenantExportEvent         `json:"sessionEvents"`
	Executions              []tenantExportExecution     `json:"executions"`
	Artifacts               []tenantExportArtifact      `json:"artifacts"`
	Credentials             []tenantExportCredential    `json:"credentials"`
	IdentityConnections     []tenantExportIdentity      `json:"identityConnections"`
	LegalHolds              []tenantExportLegalHold     `json:"legalHolds"`
	PrivacyRequests         []Request                   `json:"privacyRequests"`
	PrivacyRequestEvents    []Event                     `json:"privacyRequestEvents"`
	AuditEvents             []tenantExportAudit         `json:"auditEvents"`
}

type tenantExportTenant struct {
	ID               uuid.UUID      `json:"id"`
	Slug             string         `json:"slug"`
	Name             string         `json:"name"`
	Status           string         `json:"status"`
	LifecycleVersion int64          `json:"lifecycleVersion"`
	TrialExpiresAt   *time.Time     `json:"evaluationExpiresAt"`
	SuspendedAt      *time.Time     `json:"suspendedAt"`
	ClosedAt         *time.Time     `json:"closedAt"`
	PlanCode         string         `json:"entitlementProfileCode"`
	Region           string         `json:"region"`
	Settings         map[string]any `json:"settings"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
}

type tenantExportUser struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `json:"deletedAt"`
}

type tenantExportMembership struct {
	UserID    uuid.UUID  `json:"userId"`
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	JoinedAt  *time.Time `json:"joinedAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type tenantExportOrganization struct {
	ID                   uuid.UUID      `json:"id"`
	ParentOrganizationID *uuid.UUID     `json:"parentOrganizationId"`
	Slug                 string         `json:"slug"`
	Name                 string         `json:"name"`
	Kind                 string         `json:"kind"`
	Status               string         `json:"status"`
	Settings             map[string]any `json:"settings"`
	CreatedBy            uuid.UUID      `json:"createdBy"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
	ArchivedAt           *time.Time     `json:"archivedAt"`
}

type tenantExportOrgMembership struct {
	OrganizationID uuid.UUID `json:"organizationId"`
	UserID         uuid.UUID `json:"userId"`
	Role           string    `json:"role"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type tenantExportProject struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	Name           string     `json:"name"`
	RepositoryURL  *string    `json:"repositoryUrl"`
	DefaultBranch  string     `json:"defaultBranch"`
	Visibility     string     `json:"visibility"`
	CreatedBy      uuid.UUID  `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ArchivedAt     *time.Time `json:"archivedAt"`
}

type tenantExportSession struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	ProjectID      uuid.UUID  `json:"projectId"`
	CreatedBy      uuid.UUID  `json:"createdBy"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Visibility     string     `json:"visibility"`
	Provider       string     `json:"provider"`
	Model          *string    `json:"model"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ArchivedAt     *time.Time `json:"archivedAt"`
}

type tenantExportTurn struct {
	ID              uuid.UUID  `json:"id"`
	SessionID       uuid.UUID  `json:"sessionId"`
	CreatedBy       uuid.UUID  `json:"createdBy"`
	Status          string     `json:"status"`
	InputText       string     `json:"inputText"`
	TurnKind        string     `json:"turnKind"`
	RuntimeMode     string     `json:"runtimeMode"`
	InteractionMode string     `json:"interactionMode"`
	CreatedAt       time.Time  `json:"createdAt"`
	CompletedAt     *time.Time `json:"completedAt"`
}

type tenantExportEvent struct {
	SessionID   uuid.UUID      `json:"sessionId"`
	Sequence    int64          `json:"sequence"`
	EventID     uuid.UUID      `json:"eventId"`
	EventType   string         `json:"eventType"`
	ActorType   string         `json:"actorType"`
	ActorID     *uuid.UUID     `json:"actorId"`
	ExecutionID *uuid.UUID     `json:"executionId"`
	WorkerID    *uuid.UUID     `json:"workerId"`
	Generation  *int64         `json:"generation"`
	Payload     map[string]any `json:"payload"`
	OccurredAt  time.Time      `json:"occurredAt"`
}

type tenantExportExecution struct {
	ID                uuid.UUID  `json:"id"`
	SessionID         uuid.UUID  `json:"sessionId"`
	TurnID            uuid.UUID  `json:"turnId"`
	Status            string     `json:"status"`
	Attempt           int        `json:"attempt"`
	Generation        int64      `json:"generation"`
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TargetKind        string     `json:"targetKind"`
	Provider          *string    `json:"provider"`
	WorkerID          *uuid.UUID `json:"workerId"`
	RequestedBy       uuid.UUID  `json:"requestedBy"`
	QueuedAt          time.Time  `json:"queuedAt"`
	StartedAt         *time.Time `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	FailureCode       *string    `json:"failureCode"`
	FailureMessage    *string    `json:"failureMessage"`
}

type tenantExportArtifact struct {
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
	CreatedByType  string     `json:"createdByType"`
	CreatedByID    uuid.UUID  `json:"createdById"`
	ObjectKey      string     `json:"objectKey"`
	CreatedAt      time.Time  `json:"createdAt"`
	DeletedAt      *time.Time `json:"deletedAt"`
}

type tenantExportCredential struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID *uuid.UUID `json:"organizationId"`
	ScopeUserID    *uuid.UUID `json:"scopeUserId"`
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

type tenantExportIdentity struct {
	ID            uuid.UUID      `json:"id"`
	Kind          string         `json:"kind"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Issuer        string         `json:"issuer"`
	ClientID      *string        `json:"clientId"`
	Configuration map[string]any `json:"configuration"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

type tenantExportLegalHold struct {
	ID              uuid.UUID  `json:"id"`
	ScopeType       string     `json:"scopeType"`
	ScopeID         uuid.UUID  `json:"scopeId"`
	Name            string     `json:"name"`
	MatterReference string     `json:"matterReference"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"`
	Version         int64      `json:"version"`
	CreatedBy       uuid.UUID  `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	ReleasedBy      *uuid.UUID `json:"releasedBy"`
	ReleaseReason   *string    `json:"releaseReason"`
	ReleasedAt      *time.Time `json:"releasedAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type tenantExportAudit struct {
	exportAuditEvent
	ActorType string     `json:"actorType"`
	ActorID   *uuid.UUID `json:"actorId"`
}

func (s *Service) ExecuteTenantExport(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	httpRequestID, ipAddress string,
) (TenantExportResult, error) {
	role, err := s.requireTenant(ctx, principal, tenantID)
	if err != nil {
		return TenantExportResult{}, err
	}
	if !authorization.TenantAllows(role, authorization.RetentionManage) {
		return TenantExportResult{}, problem.New(403, "tenant_data_export_forbidden", "Tenant privacy administrator access is required.")
	}
	exportID := uuid.New()
	bundle := TenantExportBundle{
		SchemaVersion: "synara-tenant-export-v2", ExportID: exportID, GeneratedAt: s.now(),
		Users: []tenantExportUser{}, TenantMemberships: []tenantExportMembership{},
		Organizations: []tenantExportOrganization{}, OrganizationMemberships: []tenantExportOrgMembership{},
		Projects: []tenantExportProject{}, Sessions: []tenantExportSession{}, Turns: []tenantExportTurn{},
		SessionEvents: []tenantExportEvent{}, Executions: []tenantExportExecution{},
		Artifacts: []tenantExportArtifact{}, Credentials: []tenantExportCredential{},
		IdentityConnections: []tenantExportIdentity{}, LegalHolds: []tenantExportLegalHold{},
		PrivacyRequests: []Request{}, PrivacyRequestEvents: []Event{}, AuditEvents: []tenantExportAudit{},
	}
	txOptions := &sql.TxOptions{ReadOnly: true}
	if s.db.Dialector.Name() == "postgres" {
		txOptions.Isolation = sql.LevelRepeatableRead
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return loadTenantExport(ctx, tx, tenantID, &bundle)
	}, txOptions); err != nil {
		return TenantExportResult{}, err
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return TenantExportResult{}, problem.Wrap(500, "tenant_data_export_encode_failed", "Tenant data export could not be encoded.", err)
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	rowCounts := tenantExportRowCounts(bundle)
	receipt := persistence.TenantDataExport{
		ID: exportID, TenantID: tenantID, RequestedBy: principal.UserID,
		SchemaVersion: bundle.SchemaVersion, DigestSHA256: digest, ByteCount: int64(len(encoded)),
		RowCounts: rowCounts, CreatedAt: s.now(),
	}
	if err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
			return problem.Wrap(409, "tenant_data_export_receipt_failed", "Tenant data export receipt could not be recorded.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.data_exported", ResourceType: "tenant_data_export", ResourceID: &receipt.ID,
			RequestID: httpRequestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"schemaVersion": receipt.SchemaVersion, "digestSha256": digest,
				"byteCount": receipt.ByteCount, "rowCounts": rowCounts,
			},
		})
	}); err != nil {
		return TenantExportResult{}, err
	}
	return TenantExportResult{Receipt: receipt, Bundle: bundle}, nil
}

func loadTenantExport(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, bundle *TenantExportBundle) error {
	var tenant persistence.Tenant
	if err := db.WithContext(ctx).Where("id = ?", tenantID).Take(&tenant).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_tenant_failed", "Tenant data export could not load the Tenant.", err)
	}
	bundle.Tenant = tenantExportTenant{
		ID: tenant.ID, Slug: tenant.Slug, Name: tenant.Name, Status: productprofile.PublicLifecycleStatus(tenant.Status),
		LifecycleVersion: tenant.LifecycleVersion, TrialExpiresAt: tenant.TrialExpiresAt,
		SuspendedAt: tenant.SuspendedAt, ClosedAt: tenant.ClosedAt, PlanCode: productprofile.PublicProfileCode(tenant.PlanCode),
		Region: tenant.Region, Settings: tenant.Settings, CreatedAt: tenant.CreatedAt, UpdatedAt: tenant.UpdatedAt,
	}
	queries := []struct {
		code, message string
		query         *gorm.DB
	}{
		{"tenant_data_export_users_failed", "Tenant data export could not load Users.", db.WithContext(ctx).Table("users AS user_record").Select("user_record.id, user_record.email, user_record.display_name, user_record.status, user_record.created_at, user_record.updated_at, user_record.deleted_at").Joins("JOIN tenant_memberships AS membership ON membership.user_id = user_record.id AND membership.tenant_id = ?", tenantID).Order("user_record.id").Scan(&bundle.Users)},
		{"tenant_data_export_memberships_failed", "Tenant data export could not load memberships.", db.WithContext(ctx).Table("tenant_memberships").Select("user_id, role, status, joined_at, created_at, updated_at").Where("tenant_id = ?", tenantID).Order("user_id").Scan(&bundle.TenantMemberships)},
		{"tenant_data_export_org_memberships_failed", "Tenant data export could not load Organization memberships.", db.WithContext(ctx).Table("organization_memberships").Select("organization_id, user_id, role, status, created_at, updated_at").Where("tenant_id = ?", tenantID).Order("organization_id, user_id").Scan(&bundle.OrganizationMemberships)},
		{"tenant_data_export_projects_failed", "Tenant data export could not load Projects.", db.WithContext(ctx).Table("projects").Select("id, organization_id, name, repository_url, default_branch, visibility, created_by, created_at, updated_at, archived_at").Where("tenant_id = ?", tenantID).Order("id").Scan(&bundle.Projects)},
		{"tenant_data_export_sessions_failed", "Tenant data export could not load Sessions.", db.WithContext(ctx).Table("agent_sessions").Select("id, organization_id, project_id, created_by, title, status, visibility, provider, model, created_at, updated_at, archived_at").Where("tenant_id = ?", tenantID).Order("id").Scan(&bundle.Sessions)},
		{"tenant_data_export_turns_failed", "Tenant data export could not load Turns.", db.WithContext(ctx).Table("agent_turns").Select("id, session_id, created_by, status, input_text, turn_kind, runtime_mode, interaction_mode, created_at, completed_at").Where("tenant_id = ?", tenantID).Order("session_id, created_at, id").Scan(&bundle.Turns)},
		{"tenant_data_export_executions_failed", "Tenant data export could not load Executions.", db.WithContext(ctx).Table("agent_executions").Select("id, session_id, turn_id, status, attempt, generation, execution_target_id, target_kind, provider, worker_id, requested_by, queued_at, started_at, finished_at, failure_code, failure_message").Where("tenant_id = ?", tenantID).Order("queued_at, id").Scan(&bundle.Executions)},
		{"tenant_data_export_artifacts_failed", "Tenant data export could not load Artifacts.", db.WithContext(ctx).Table("artifacts").Select("id, organization_id, project_id, session_id, execution_id, kind, status, original_name, content_type, size_bytes, sha256, created_by_type, created_by_id, object_key, created_at, deleted_at").Where("tenant_id = ?", tenantID).Order("created_at, id").Scan(&bundle.Artifacts)},
		{"tenant_data_export_credentials_failed", "Tenant data export could not load Credential metadata.", db.WithContext(ctx).Table("provider_credentials").Select("id, organization_id, scope_user_id, name, purpose, provider, credential_type, scope, version, created_at, updated_at, expires_at, revoked_at").Where("tenant_id = ?", tenantID).Order("created_at, id").Scan(&bundle.Credentials)},
		{"tenant_data_export_holds_failed", "Tenant data export could not load Legal Holds.", db.WithContext(ctx).Table("legal_holds").Select("id, scope_type, scope_id, name, matter_reference, reason, status, version, created_by, created_at, released_by, release_reason, released_at, updated_at").Where("tenant_id = ?", tenantID).Order("created_at, id").Scan(&bundle.LegalHolds)},
	}
	for _, query := range queries {
		if err := query.query.Error; err != nil {
			return problem.Wrap(500, query.code, query.message, err)
		}
	}
	var organizationModels []persistence.Organization
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("id").Find(&organizationModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_organizations_failed", "Tenant data export could not load Organizations.", err)
	}
	for _, model := range organizationModels {
		bundle.Organizations = append(bundle.Organizations, tenantExportOrganization{
			ID: model.ID, ParentOrganizationID: model.ParentOrganizationID, Slug: model.Slug,
			Name: model.Name, Kind: model.Kind, Status: model.Status, Settings: model.Settings,
			CreatedBy: model.CreatedBy, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
			ArchivedAt: model.ArchivedAt,
		})
	}
	var identityModels []persistence.IdentityConnection
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at, id").Find(&identityModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_identity_failed", "Tenant data export could not load Identity Connections.", err)
	}
	for _, model := range identityModels {
		bundle.IdentityConnections = append(bundle.IdentityConnections, tenantExportIdentity{
			ID: model.ID, Kind: model.Kind, Name: model.Name, Status: model.Status,
			Issuer: model.Issuer, ClientID: model.ClientID, Configuration: model.Configuration,
			CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
		})
	}
	var eventModels []persistence.SessionEvent
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("session_id, sequence").Find(&eventModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_events_failed", "Tenant data export could not load Session Events.", err)
	}
	for _, model := range eventModels {
		bundle.SessionEvents = append(bundle.SessionEvents, tenantExportEvent{
			SessionID: model.SessionID, Sequence: model.Sequence, EventID: model.EventID,
			EventType: model.EventType, ActorType: model.ActorType, ActorID: model.ActorID,
			ExecutionID: model.ExecutionID, WorkerID: model.WorkerID, Generation: model.Generation,
			Payload: model.Payload, OccurredAt: model.OccurredAt,
		})
	}
	var privacyModels []persistence.PrivacyRequest
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at, id").Find(&privacyModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_privacy_failed", "Tenant data export could not load Privacy Requests.", err)
	}
	for _, model := range privacyModels {
		bundle.PrivacyRequests = append(bundle.PrivacyRequests, toRequest(model))
	}
	var privacyEventModels []persistence.PrivacyRequestEvent
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("privacy_request_id, version").Find(&privacyEventModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_privacy_history_failed", "Tenant data export could not load Privacy Request history.", err)
	}
	for _, model := range privacyEventModels {
		bundle.PrivacyRequestEvents = append(bundle.PrivacyRequestEvents, toEvent(model))
	}
	var auditModels []persistence.AuditLog
	if err := db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("occurred_at, event_id").Find(&auditModels).Error; err != nil {
		return problem.Wrap(500, "tenant_data_export_audit_failed", "Tenant data export could not load Audit events.", err)
	}
	for _, model := range auditModels {
		bundle.AuditEvents = append(bundle.AuditEvents, tenantExportAudit{
			exportAuditEvent: exportAuditEvent{
				EventID: model.EventID, Action: model.Action, ResourceType: model.ResourceType,
				ResourceID: model.ResourceID, OrganizationID: model.OrganizationID,
				RequestID: model.RequestID, Metadata: model.Metadata, OccurredAt: model.OccurredAt,
			},
			ActorType: model.ActorType, ActorID: model.ActorID,
		})
	}
	return nil
}

func tenantExportRowCounts(bundle TenantExportBundle) map[string]any {
	return map[string]any{
		"users": len(bundle.Users), "tenantMemberships": len(bundle.TenantMemberships),
		"organizations": len(bundle.Organizations), "organizationMemberships": len(bundle.OrganizationMemberships),
		"projects": len(bundle.Projects), "sessions": len(bundle.Sessions), "turns": len(bundle.Turns),
		"sessionEvents": len(bundle.SessionEvents), "executions": len(bundle.Executions),
		"artifacts": len(bundle.Artifacts), "credentials": len(bundle.Credentials),
		"identityConnections": len(bundle.IdentityConnections), "legalHolds": len(bundle.LegalHolds),
		"privacyRequests": len(bundle.PrivacyRequests), "privacyRequestEvents": len(bundle.PrivacyRequestEvents),
		"auditEvents": len(bundle.AuditEvents),
	}
}
