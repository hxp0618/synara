package memories

import (
	"context"
	"mime"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const maximumEffectiveMemoryHeads = executions.MaximumRecoveryMemoryReferences

var maximumCandidateMemoryHeads = maximumEffectiveMemoryHeads * 3

var (
	memoryKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,159}$`)
	memoryHeadIDSpace = uuid.MustParse("31bf8752-689c-57ff-b6ad-ac3d2090c1bf")
	memoryRevisionIDs = uuid.MustParse("98601b7e-bccc-52e3-a4bd-6bac3a46fe0c")
)

type PublishInput struct {
	ScopeType       string    `json:"scopeType"`
	ScopeID         uuid.UUID `json:"scopeId"`
	MemoryKey       string    `json:"memoryKey"`
	ArtifactID      uuid.UUID `json:"artifactId"`
	SHA256          string    `json:"sha256"`
	ExpectedVersion int64     `json:"expectedVersion"`
}

type Publication struct {
	HeadID         uuid.UUID `json:"headId"`
	TenantID       uuid.UUID `json:"tenantId"`
	ScopeType      string    `json:"scopeType"`
	ScopeID        uuid.UUID `json:"scopeId"`
	MemoryKey      string    `json:"memoryKey"`
	RevisionID     uuid.UUID `json:"revisionId"`
	RevisionNumber int64     `json:"revisionNumber"`
	ArtifactID     uuid.UUID `json:"artifactId"`
	SHA256         string    `json:"sha256"`
	MediaType      string    `json:"mediaType"`
	SizeBytes      int64     `json:"sizeBytes"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Publish(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input PublishInput,
	requestID, ipAddress string,
) (Publication, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return Publication{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	input.ScopeType = strings.ToLower(strings.TrimSpace(input.ScopeType))
	input.MemoryKey = strings.ToLower(strings.TrimSpace(input.MemoryKey))
	input.SHA256 = strings.ToLower(strings.TrimSpace(input.SHA256))
	if input.ScopeID == uuid.Nil || input.ArtifactID == uuid.Nil {
		return Publication{}, problem.New(400, "invalid_memory_reference", "scopeId and artifactId are required.")
	}
	if !memoryKeyPattern.MatchString(input.MemoryKey) {
		return Publication{}, problem.New(400, "invalid_memory_key", "memoryKey must use lowercase letters, digits, dots, underscores, or hyphens.")
	}
	if !sha256Pattern.MatchString(input.SHA256) {
		return Publication{}, problem.New(400, "invalid_memory_sha256", "sha256 must be a lowercase SHA-256 digest.")
	}
	if input.ExpectedVersion < 0 {
		return Publication{}, problem.New(400, "invalid_memory_version", "expectedVersion must not be negative.")
	}
	if err := s.authorizeScope(ctx, principal, tenantID, input); err != nil {
		return Publication{}, err
	}

	headID := memoryHeadID(tenantID, input.ScopeType, input.ScopeID, input.MemoryKey)
	var result Publication
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := s.now()
		head, err := ensureMemoryHead(ctx, tx, tenantID, principal.UserID, headID, input, now)
		if err != nil {
			return err
		}
		if head.CurrentRevisionID != nil {
			current, loadErr := loadRevision(ctx, tx, tenantID, head.ID, *head.CurrentRevisionID)
			if loadErr != nil {
				return loadErr
			}
			if current.RevisionNumber != head.Version {
				return problem.New(409, "memory_revision_mismatch", "The Agent Memory Head and immutable Revision version do not match.")
			}
			if current.ArtifactID == input.ArtifactID && current.SHA256 == input.SHA256 {
				artifact, validationErr := validateArtifact(ctx, tx, head, current.ArtifactID, current.SHA256)
				if validationErr != nil {
					return validationErr
				}
				if validationErr := validateRevisionArtifactIdentity(current, artifact); validationErr != nil {
					return validationErr
				}
				result = publication(head, current)
				return nil
			}
		}
		if head.Version != input.ExpectedVersion {
			return problem.New(409, "memory_version_conflict", "The Memory Head changed; reload it before publishing another Revision.")
		}
		artifact, err := validateArtifact(ctx, tx, head, input.ArtifactID, input.SHA256)
		if err != nil {
			return err
		}

		nextVersion := head.Version + 1
		revision := persistence.AgentMemoryRevision{
			ID: memoryRevisionID(head.ID, nextVersion), TenantID: tenantID,
			MemoryHeadID: head.ID, RevisionNumber: nextVersion,
			ArtifactID: input.ArtifactID, SHA256: input.SHA256,
			MediaType: artifact.mediaType, SizeBytes: artifact.sizeBytes,
			CreatedBy: principal.UserID, CreatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&revision).Error; err != nil {
			return problem.Wrap(409, "memory_revision_create_rejected", "The immutable Memory Revision could not be published.", err)
		}
		updated := tx.WithContext(ctx).Model(&persistence.AgentMemoryHead{}).
			Where("tenant_id = ? AND id = ? AND version = ?", tenantID, head.ID, head.Version).
			Updates(map[string]any{
				"current_revision_id": revision.ID, "version": nextVersion, "enabled": true,
				"updated_by": principal.UserID, "updated_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "memory_head_publish_conflict", "The Memory Head changed while publishing its Revision.", updated.Error)
		}
		head.CurrentRevisionID = &revision.ID
		head.Version = nextVersion
		head.Enabled = true
		head.UpdatedBy = principal.UserID
		head.UpdatedAt = now
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "agent_memory.published", ResourceType: "agent_memory_revision", ResourceID: &revision.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"scopeType": head.ScopeType, "scopeId": input.ScopeID,
				"memoryKey": head.MemoryKey, "headId": head.ID,
				"revisionNumber": revision.RevisionNumber, "artifactId": revision.ArtifactID,
				"sha256": revision.SHA256, "mediaType": revision.MediaType,
				"sizeBytes": revision.SizeBytes,
			},
		}); err != nil {
			return err
		}
		result = publication(head, revision)
		return nil
	})
	return result, err
}

func (s *Service) ListEffectiveForSession(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) ([]executions.RecoveryMemoryReference, error) {
	tenantID, session, err := s.authorizeSessionRead(ctx, principal, sessionID)
	if err != nil {
		return nil, err
	}
	return s.resolveRecoveryMemoryReferencesForScope(ctx, s.db, tenantID, recoveryMemoryScope{
		sessionID: session.ID, projectID: session.ProjectID, userID: &principal.UserID,
	})
}

// ResolveRecoveryMemoryReferences returns a deterministic effective set. A
// lower scope overrides the same key from a broader scope, while every chosen
// Revision and Artifact is revalidated inside the caller's Claim transaction.
// Without an authoritative actor, user-scoped Memory is excluded.
func (s *Service) ResolveRecoveryMemoryReferences(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
) ([]executions.RecoveryMemoryReference, error) {
	scope, err := s.loadRecoveryMemorySessionScope(ctx, tx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	return s.resolveRecoveryMemoryReferencesForScope(ctx, tx, tenantID, scope)
}

func (s *Service) ResolveExecutionRecoveryMemoryReferences(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, executionID uuid.UUID,
) ([]executions.RecoveryMemoryReference, error) {
	scope, err := s.loadRecoveryMemoryExecutionScope(ctx, tx, tenantID, executionID)
	if err != nil {
		return nil, err
	}
	return s.resolveRecoveryMemoryReferencesForScope(ctx, tx, tenantID, scope)
}

type recoveryMemoryScope struct {
	sessionID uuid.UUID
	projectID uuid.UUID
	userID    *uuid.UUID
}

func (s *Service) loadRecoveryMemorySessionScope(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
) (recoveryMemoryScope, error) {
	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "project_id").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).Take(&session).Error; err != nil {
		return recoveryMemoryScope{}, problem.Wrap(409, "memory_session_unavailable", "The Session Memory scope is unavailable.", err)
	}
	return recoveryMemoryScope{sessionID: session.ID, projectID: session.ProjectID}, nil
}

func (s *Service) loadRecoveryMemoryExecutionScope(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, executionID uuid.UUID,
) (recoveryMemoryScope, error) {
	var row struct {
		SessionID     uuid.UUID `gorm:"column:session_id"`
		ProjectID     uuid.UUID `gorm:"column:project_id"`
		RequestedBy   uuid.UUID `gorm:"column:requested_by"`
		TurnCreatedBy uuid.UUID `gorm:"column:turn_created_by"`
	}
	if err := tx.WithContext(ctx).
		Table("agent_executions AS e").
		Select("e.session_id, s.project_id, e.requested_by, t.created_by AS turn_created_by").
		Joins("JOIN agent_sessions AS s ON s.tenant_id = e.tenant_id AND s.id = e.session_id").
		Joins("JOIN agent_turns AS t ON t.tenant_id = e.tenant_id AND t.session_id = e.session_id AND t.id = e.turn_id").
		Where("e.tenant_id = ? AND e.id = ?", tenantID, executionID).
		Take(&row).Error; err != nil {
		return recoveryMemoryScope{}, problem.Wrap(409, "memory_execution_unavailable", "The Execution Memory scope is unavailable.", err)
	}
	return recoveryMemoryScope{
		sessionID: row.SessionID,
		projectID: row.ProjectID,
		userID:    resolveExecutionMemoryUserID(row.RequestedBy, row.TurnCreatedBy),
	}, nil
}

func (s *Service) resolveRecoveryMemoryReferencesForScope(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	scope recoveryMemoryScope,
) ([]executions.RecoveryMemoryReference, error) {
	predicate, args := memoryHeadScopePredicate(scope)
	heads := make([]persistence.AgentMemoryHead, 0)
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND enabled = ?", tenantID, true).
		Where(predicate, args...).
		Order("scope_type, memory_key, id").Limit(maximumCandidateMemoryHeads + 1).Find(&heads).Error; err != nil {
		return nil, problem.Wrap(500, "memory_heads_load_failed", "The effective Agent Memory Heads could not be loaded.", err)
	}
	if len(heads) > maximumCandidateMemoryHeads {
		return nil, problem.New(409, "memory_head_limit_exceeded", "The Session resolves more Agent Memory Heads than one Recovery Bundle can safely include.")
	}

	winners := make(map[string]persistence.AgentMemoryHead, len(heads))
	for _, head := range heads {
		current, found := winners[head.MemoryKey]
		if !found || memoryScopePriority(head.ScopeType) > memoryScopePriority(current.ScopeType) {
			winners[head.MemoryKey] = head
		}
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maximumEffectiveMemoryHeads {
		return nil, problem.New(409, "memory_head_limit_exceeded", "The Session resolves more Agent Memory Heads than one Recovery Bundle can safely include.")
	}

	references := make([]executions.RecoveryMemoryReference, 0, len(keys))
	var totalBytes int64
	for _, key := range keys {
		head := winners[key]
		if head.CurrentRevisionID == nil || head.Version <= 0 {
			return nil, problem.New(409, "memory_head_invalid", "An enabled Agent Memory Head has no published Revision.")
		}
		revision, err := loadRevision(ctx, tx, tenantID, head.ID, *head.CurrentRevisionID)
		if err != nil {
			return nil, err
		}
		if revision.RevisionNumber != head.Version {
			return nil, problem.New(409, "memory_revision_mismatch", "The Agent Memory Head and immutable Revision version do not match.")
		}
		artifact, err := validateArtifact(ctx, tx, head, revision.ArtifactID, revision.SHA256)
		if err != nil {
			return nil, err
		}
		if err := validateRevisionArtifactIdentity(revision, artifact); err != nil {
			return nil, err
		}
		totalBytes += revision.SizeBytes
		if totalBytes > executions.MaximumRecoveryMemoryTotalBytes {
			return nil, problem.New(409, "memory_total_size_exceeded", "The effective Agent Memory set exceeds the Recovery Bundle size limit.")
		}
		references = append(references, executions.RecoveryMemoryReference{
			Scope: head.ScopeType, ScopeID: memoryScopeID(head), HeadID: head.ID,
			MemoryKey: head.MemoryKey, RevisionID: revision.ID,
			ArtifactID: revision.ArtifactID, SHA256: revision.SHA256,
			MediaType: revision.MediaType, SizeBytes: revision.SizeBytes,
		})
	}
	return references, nil
}

func memoryHeadScopePredicate(scope recoveryMemoryScope) (string, []any) {
	clauses := []string{
		"(scope_type = 'project' AND scope_project_id = ?)",
		"(scope_type = 'session' AND scope_session_id = ?)",
	}
	args := []any{scope.projectID, scope.sessionID}
	if scope.userID != nil && *scope.userID != uuid.Nil {
		clauses = append([]string{"(scope_type = 'user' AND scope_user_id = ?)"}, clauses...)
		args = append([]any{*scope.userID}, args...)
	}
	return strings.Join(clauses, " OR "), args
}

func resolveExecutionMemoryUserID(requestedBy, turnCreatedBy uuid.UUID) *uuid.UUID {
	switch {
	case requestedBy != uuid.Nil:
		userID := requestedBy
		return &userID
	case turnCreatedBy != uuid.Nil:
		userID := turnCreatedBy
		return &userID
	default:
		return nil
	}
}

func (s *Service) authorizeScope(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input PublishInput,
) error {
	switch input.ScopeType {
	case "user":
		if input.ScopeID != principal.UserID {
			return problem.New(404, "memory_scope_not_found", "Memory scope not found.")
		}
		_, err := s.authorizer.TenantRole(ctx, principal.UserID, tenantID)
		return err
	case "project":
		var project persistence.Project
		if err := s.db.WithContext(ctx).
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, input.ScopeID).
			Take(&project).Error; err != nil {
			return problem.Wrap(404, "memory_scope_not_found", "Memory scope not found.", err)
		}
		_, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, project.OrganizationID, authorization.ProjectUpdate)
		return err
	case "session":
		_, session, err := s.authorizeSessionRead(ctx, principal, input.ScopeID)
		if err != nil {
			return err
		}
		_, err = s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, session.OrganizationID, authorization.ArtifactWrite)
		return err
	default:
		return problem.New(400, "invalid_memory_scope", "scopeType must be user, project, or session.")
	}
}

func (s *Service) authorizeSessionRead(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (uuid.UUID, persistence.AgentSession, error) {
	if principal.ActiveTenantID == nil {
		return uuid.Nil, persistence.AgentSession{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	tenantID := *principal.ActiveTenantID
	var session persistence.AgentSession
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, sessionID).
		Take(&session).Error; err != nil {
		return uuid.Nil, persistence.AgentSession{}, problem.Wrap(404, "session_not_found", "Session not found.", err)
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, session.OrganizationID, authorization.SessionRead)
	if err != nil {
		return uuid.Nil, persistence.AgentSession{}, err
	}
	if session.Visibility == "private" && session.CreatedBy != principal.UserID &&
		!authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		return uuid.Nil, persistence.AgentSession{}, problem.New(404, "session_not_found", "Session not found.")
	}
	return tenantID, session, nil
}

func ensureMemoryHead(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, userID, headID uuid.UUID,
	input PublishInput,
	now time.Time,
) (persistence.AgentMemoryHead, error) {
	head := persistence.AgentMemoryHead{
		ID: headID, TenantID: tenantID, ScopeType: input.ScopeType, MemoryKey: input.MemoryKey,
		Version: 0, Enabled: false, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	switch input.ScopeType {
	case "user":
		head.ScopeUserID = &input.ScopeID
	case "project":
		head.ScopeProjectID = &input.ScopeID
	case "session":
		head.ScopeSessionID = &input.ScopeID
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&head).Error; err != nil {
		return persistence.AgentMemoryHead{}, problem.Wrap(409, "memory_head_create_rejected", "The Agent Memory Head could not be created.", err)
	}
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND id = ?", tenantID, headID).Take(&head).Error; err != nil {
		return persistence.AgentMemoryHead{}, problem.Wrap(500, "memory_head_load_failed", "The Agent Memory Head could not be locked.", err)
	}
	if head.ScopeType != input.ScopeType || head.MemoryKey != input.MemoryKey || memoryScopeID(head) != input.ScopeID {
		return persistence.AgentMemoryHead{}, problem.New(409, "memory_head_identity_conflict", "The deterministic Agent Memory Head identity is already bound to another scope.")
	}
	return head, nil
}

func loadRevision(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, headID, revisionID uuid.UUID,
) (persistence.AgentMemoryRevision, error) {
	var revision persistence.AgentMemoryRevision
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND memory_head_id = ? AND id = ?", tenantID, headID, revisionID).
		Take(&revision).Error; err != nil {
		return persistence.AgentMemoryRevision{}, problem.Wrap(409, "memory_revision_unavailable", "The immutable Agent Memory Revision is unavailable.", err)
	}
	return revision, nil
}

type validatedMemoryArtifact struct {
	mediaType string
	sizeBytes int64
}

func validateArtifact(
	ctx context.Context,
	tx *gorm.DB,
	head persistence.AgentMemoryHead,
	artifactID uuid.UUID,
	sha256 string,
) (validatedMemoryArtifact, error) {
	var artifact persistence.Artifact
	if err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND id = ? AND kind = ? AND status = ? AND deleted_at IS NULL AND sha256 = ?",
			head.TenantID, artifactID, "memory", "ready", sha256).
		Take(&artifact).Error; err != nil {
		return validatedMemoryArtifact{}, problem.Wrap(409, "memory_artifact_unavailable", "The Memory Artifact is unavailable or does not match its immutable SHA-256.", err)
	}
	if artifact.SizeBytes == nil || artifact.ContentType == nil || *artifact.SizeBytes < 0 {
		return validatedMemoryArtifact{}, problem.New(409, "memory_artifact_metadata_invalid", "The Memory Artifact must declare a ready size and content type.")
	}
	if *artifact.SizeBytes > executions.MaximumRecoveryMemoryArtifactBytes {
		return validatedMemoryArtifact{}, problem.New(409, "memory_artifact_too_large", "The Memory Artifact exceeds the per-document Recovery Bundle size limit.")
	}
	mediaType, err := normalizeRecoveryMemoryMediaType(*artifact.ContentType)
	if err != nil {
		return validatedMemoryArtifact{}, err
	}
	switch head.ScopeType {
	case "user":
		if head.ScopeUserID == nil || artifact.CreatedByType != "user" || artifact.CreatedByID != *head.ScopeUserID {
			return validatedMemoryArtifact{}, problem.New(409, "memory_artifact_scope_mismatch", "The Memory Artifact does not belong to its User scope.")
		}
	case "project":
		if head.ScopeProjectID == nil || artifact.ProjectID != *head.ScopeProjectID {
			return validatedMemoryArtifact{}, problem.New(409, "memory_artifact_scope_mismatch", "The Memory Artifact does not belong to its Project scope.")
		}
	case "session":
		if head.ScopeSessionID == nil || artifact.SessionID != *head.ScopeSessionID {
			return validatedMemoryArtifact{}, problem.New(409, "memory_artifact_scope_mismatch", "The Memory Artifact does not belong to its Session scope.")
		}
	default:
		return validatedMemoryArtifact{}, problem.New(409, "memory_scope_invalid", "The Memory Head scope is invalid.")
	}
	// This layer validates only ready metadata. Artifact bytes, digest replay,
	// and UTF-8 content are verified later by agentd when the Memory document is
	// downloaded for injection; this path does not read object contents.
	return validatedMemoryArtifact{mediaType: mediaType, sizeBytes: *artifact.SizeBytes}, nil
}

func normalizeRecoveryMemoryMediaType(contentType string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if err != nil || !supportedRecoveryMemoryMediaType(mediaType) {
		return "", problem.New(409, "memory_artifact_content_type_unsupported", "The Memory Artifact content type is unsupported for Recovery Bundles.")
	}
	return mediaType, nil
}

func supportedRecoveryMemoryMediaType(mediaType string) bool {
	switch mediaType {
	case "text/plain", "text/markdown", "application/json":
		return true
	default:
		return false
	}
}

func validateRevisionArtifactIdentity(
	revision persistence.AgentMemoryRevision,
	artifact validatedMemoryArtifact,
) error {
	if revision.MediaType != artifact.mediaType || revision.SizeBytes != artifact.sizeBytes {
		return problem.New(
			409,
			"memory_revision_metadata_mismatch",
			"The immutable Memory Revision media type or byte size no longer matches its Artifact.",
		)
	}
	return nil
}

func memoryHeadID(tenantID uuid.UUID, scopeType string, scopeID uuid.UUID, memoryKey string) uuid.UUID {
	return uuid.NewSHA1(memoryHeadIDSpace, []byte(tenantID.String()+"\x00"+scopeType+"\x00"+scopeID.String()+"\x00"+memoryKey))
}

func memoryRevisionID(headID uuid.UUID, version int64) uuid.UUID {
	return uuid.NewSHA1(memoryRevisionIDs, []byte(headID.String()+"\x00"+strconv.FormatInt(version, 10)))
}

func memoryScopeID(head persistence.AgentMemoryHead) uuid.UUID {
	switch head.ScopeType {
	case "user":
		if head.ScopeUserID != nil {
			return *head.ScopeUserID
		}
	case "project":
		if head.ScopeProjectID != nil {
			return *head.ScopeProjectID
		}
	case "session":
		if head.ScopeSessionID != nil {
			return *head.ScopeSessionID
		}
	}
	return uuid.Nil
}

func memoryScopePriority(scope string) int {
	switch scope {
	case "session":
		return 3
	case "project":
		return 2
	case "user":
		return 1
	default:
		return 0
	}
}

func publication(head persistence.AgentMemoryHead, revision persistence.AgentMemoryRevision) Publication {
	return Publication{
		HeadID: head.ID, TenantID: head.TenantID, ScopeType: head.ScopeType,
		ScopeID: memoryScopeID(head), MemoryKey: head.MemoryKey,
		RevisionID: revision.ID, RevisionNumber: revision.RevisionNumber,
		ArtifactID: revision.ArtifactID, SHA256: revision.SHA256,
		MediaType: revision.MediaType, SizeBytes: revision.SizeBytes,
		Enabled: head.Enabled, CreatedAt: revision.CreatedAt, UpdatedAt: head.UpdatedAt,
	}
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
