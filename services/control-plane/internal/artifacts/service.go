package artifacts

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

var allowedKinds = map[string]struct{}{
	"attachment": {}, "generated_file": {}, "terminal_log": {},
	"workspace_snapshot": {}, "checkpoint": {},
}

type Service struct {
	db             *gorm.DB
	store          Store
	authorizer     *authorization.Authorizer
	executions     *executions.Service
	sessions       *sessions.Service
	presignTTL     time.Duration
	maxUploadBytes int64
	now            func() time.Time
}

func NewService(db *gorm.DB, store Store, cfg config.Config, executionService *executions.Service, sessionService *sessions.Service) *Service {
	return &Service{
		db: db, store: store, authorizer: authorization.NewAuthorizer(db), executions: executionService,
		sessions: sessionService, presignTTL: cfg.ArtifactPresignTTL,
		maxUploadBytes: cfg.ArtifactMaxUploadBytes, now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) CheckStore(ctx context.Context) error {
	return s.store.Check(ctx)
}

func (s *Service) Create(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	input CreateInput,
	requestID, ipAddress string,
) (UploadGrant, error) {
	session, err := s.authorizeSession(ctx, principal, sessionID, authorization.ArtifactWrite, true)
	if err != nil {
		return UploadGrant{}, err
	}
	normalized, err := s.normalizeCreate(input)
	if err != nil {
		return UploadGrant{}, err
	}
	if normalized.ExecutionID != nil {
		if err := s.requireExecution(ctx, session.TenantID, session.ID, *normalized.ExecutionID); err != nil {
			return UploadGrant{}, err
		}
	}
	model, plainToken, err := s.pendingModel(session, normalized, "user", principal.UserID)
	if err != nil {
		return UploadGrant{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "artifact_create_rejected", "Artifact creation was rejected by an isolation constraint.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: model.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "artifact.created", ResourceType: "artifact", ResourceID: &model.ID,
			OrganizationID: &model.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"sessionId": model.SessionID, "kind": model.Kind},
		})
	})
	if err != nil {
		return UploadGrant{}, err
	}
	return s.uploadGrant(ctx, model, plainToken)
}

func (s *Service) CreateForWorker(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input WorkerCreateInput,
) (UploadGrant, error) {
	normalized, err := s.normalizeCreate(CreateInput{Kind: input.Kind, OriginalName: input.OriginalName, ExecutionID: &executionID, ExpiresAt: input.ExpiresAt})
	if err != nil {
		return UploadGrant{}, err
	}
	var model persistence.Artifact
	var plainToken string
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		execution, err := s.executions.AuthorizeArtifactWrite(ctx, tx, worker, executionID, executions.LeaseInput{
			TenantID: input.TenantID, Generation: input.Generation, LeaseToken: input.LeaseToken,
		})
		if err != nil {
			return err
		}
		var session persistence.AgentSession
		if err := tx.WithContext(ctx).
			Where("tenant_id = ? AND id = ? AND status = ?", execution.TenantID, execution.SessionID, "active").
			Take(&session).Error; err != nil {
			return problem.Wrap(409, "artifact_session_unavailable", "The execution session is unavailable for artifact upload.", err)
		}
		model, plainToken, err = s.pendingModel(session, normalized, "worker", worker.ID)
		if err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "artifact_create_rejected", "Artifact creation was rejected by an isolation constraint.", err)
		}
		return nil
	})
	if err != nil {
		return UploadGrant{}, err
	}
	return s.uploadGrant(ctx, model, plainToken)
}

func (s *Service) uploadGrant(ctx context.Context, model persistence.Artifact, plainToken string) (UploadGrant, error) {
	grant := UploadGrant{
		Artifact: toArtifact(model), Method: "PUT", Headers: map[string]string{}, ExpiresAt: *model.UploadExpiresAt,
	}
	if s.store.IsLocal() {
		grant.URL = "/v1/artifact-content/" + model.ID.String() + "?token=" + url.QueryEscape(plainToken)
		return grant, nil
	}
	uploadKey := model.ObjectKey
	if model.UploadObjectKey != nil {
		uploadKey = *model.UploadObjectKey
	}
	signed, err := s.store.PresignUpload(ctx, uploadKey, s.presignTTL)
	if err != nil {
		_ = s.db.WithContext(ctx).Model(&persistence.Artifact{}).Where("id = ? AND status = ?", model.ID, "pending").Update("status", "failed").Error
		return UploadGrant{}, problem.Wrap(503, "artifact_upload_signing_failed", "Failed to issue an artifact upload URL.", err)
	}
	grant.URL = signed
	return grant, nil
}

func (s *Service) UploadLocal(
	ctx context.Context,
	artifactID uuid.UUID,
	plainToken string,
	contentType string,
	contentLength int64,
	reader io.Reader,
) error {
	if !s.store.IsLocal() {
		return problem.New(404, "not_found", "Route not found.")
	}
	contentType, err := normalizeContentType(contentType)
	if err != nil {
		return err
	}
	if contentLength > s.maxUploadBytes {
		return problem.New(413, "artifact_too_large", "Artifact upload exceeds the configured size limit.")
	}
	var model persistence.Artifact
	err = s.db.WithContext(ctx).Where("id = ? AND status = ?", artifactID, "pending").Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(404, "artifact_upload_not_found", "Artifact upload is unavailable.")
	}
	if err != nil {
		return problem.Wrap(500, "artifact_upload_lookup_failed", "Failed to load the pending artifact upload.", err)
	}
	if model.UploadExpiresAt == nil || !model.UploadExpiresAt.After(s.now()) || len(model.UploadTokenHash) == 0 ||
		subtle.ConstantTimeCompare(model.UploadTokenHash, secret.HashToken(strings.TrimSpace(plainToken))) != 1 {
		return problem.New(401, "invalid_artifact_upload_token", "The artifact upload token is invalid or expired.")
	}
	consume := s.db.WithContext(ctx).Model(&persistence.Artifact{}).
		Where("id = ? AND status = ? AND upload_token_hash = ? AND upload_expires_at > ?", model.ID, "pending", model.UploadTokenHash, s.now()).
		Update("upload_token_hash", nil)
	if consume.Error != nil {
		return problem.Wrap(500, "artifact_upload_token_consume_failed", "Failed to reserve the artifact upload.", consume.Error)
	}
	if consume.RowsAffected != 1 {
		return problem.New(409, "artifact_upload_in_progress", "The artifact upload token has already been used.")
	}

	limited := io.LimitReader(reader, s.maxUploadBytes+1)
	info, putErr := s.store.Put(ctx, model.ObjectKey, limited, contentLength, contentType)
	if putErr == nil && info.Size > s.maxUploadBytes {
		putErr = problem.New(413, "artifact_too_large", "Artifact upload exceeds the configured size limit.")
	}
	if putErr != nil {
		_ = s.store.Delete(ctx, model.ObjectKey)
		_ = s.db.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND status = ? AND upload_token_hash IS NULL", model.ID, "pending").
			Update("upload_token_hash", model.UploadTokenHash).Error
		var apiError *problem.Error
		if errors.As(putErr, &apiError) {
			return apiError
		}
		return problem.Wrap(500, "artifact_upload_failed", "Failed to store the artifact payload.", putErr)
	}
	updates := map[string]any{"size_bytes": info.Size, "content_type": contentType}
	if info.Version != "" {
		updates["object_version"] = info.Version
	}
	result := s.db.WithContext(ctx).Model(&persistence.Artifact{}).
		Where("id = ? AND status = ? AND upload_token_hash IS NULL", model.ID, "pending").Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		_ = s.store.Delete(ctx, model.ObjectKey)
		return problem.Wrap(409, "artifact_upload_commit_failed", "Failed to commit the artifact upload metadata.", result.Error)
	}
	return nil
}

func (s *Service) Complete(
	ctx context.Context,
	principal identity.Principal,
	artifactID uuid.UUID,
	input CompleteInput,
	requestID, ipAddress string,
) (Artifact, error) {
	model, err := s.authorizedArtifact(ctx, principal, artifactID, authorization.ArtifactWrite)
	if err != nil {
		return Artifact{}, err
	}
	if model.CreatedByType == "worker" {
		return Artifact{}, problem.New(403, "worker_artifact_confirmation_required", "Worker-created artifacts must be confirmed through the current execution lease.")
	}
	return s.complete(ctx, model, input, nil, &principal.UserID, requestID, ipAddress)
}

func (s *Service) CompleteForWorker(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID, artifactID uuid.UUID,
	input WorkerCompleteInput,
) (Artifact, error) {
	var model persistence.Artifact
	err := s.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND execution_id = ? AND created_by_type = ? AND created_by_id = ?", artifactID, input.TenantID, executionID, "worker", worker.ID).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Artifact{}, problem.New(404, "artifact_not_found", "Artifact not found.")
	}
	if err != nil {
		return Artifact{}, problem.Wrap(500, "artifact_load_failed", "Failed to load the artifact.", err)
	}
	lease := &workerLeaseConfirmation{worker: worker, executionID: executionID, input: executions.LeaseInput{
		TenantID: input.TenantID, Generation: input.Generation, LeaseToken: input.LeaseToken,
	}}
	return s.complete(ctx, model, input.CompleteInput, lease, nil, "", "")
}

type workerLeaseConfirmation struct {
	worker      persistence.WorkerInstance
	executionID uuid.UUID
	input       executions.LeaseInput
}

func (s *Service) complete(
	ctx context.Context,
	model persistence.Artifact,
	input CompleteInput,
	workerLease *workerLeaseConfirmation,
	userActorID *uuid.UUID,
	requestID, ipAddress string,
) (Artifact, error) {
	input, err := normalizeComplete(input, s.maxUploadBytes)
	if err != nil {
		return Artifact{}, err
	}
	if model.Status == "ready" {
		if model.SizeBytes != nil && *model.SizeBytes == input.SizeBytes && model.SHA256 != nil && *model.SHA256 == input.SHA256 &&
			model.ContentType != nil && *model.ContentType == input.ContentType {
			s.cleanupPromotedUpload(ctx, model)
			return toArtifact(model), nil
		}
		return Artifact{}, problem.New(409, "artifact_already_ready", "Artifact is already ready with different metadata.")
	}
	if model.Status != "pending" {
		return Artifact{}, problem.New(409, "artifact_not_pending", "Artifact is not pending upload confirmation.")
	}
	if model.UploadExpiresAt == nil || !model.UploadExpiresAt.After(s.now()) {
		return Artifact{}, problem.New(410, "artifact_upload_expired", "Artifact upload confirmation has expired.")
	}
	uploadKey := model.ObjectKey
	if model.UploadObjectKey != nil {
		uploadKey = *model.UploadObjectKey
	}
	info, err := s.verifyObject(ctx, uploadKey, input, model.ContentType)
	if err != nil {
		return Artifact{}, err
	}
	if uploadKey != model.ObjectKey {
		reader, err := s.store.Open(ctx, uploadKey)
		if err != nil {
			return Artifact{}, problem.Wrap(409, "artifact_object_unreadable", "The verified upload object cannot be promoted.", err)
		}
		info, err = s.store.Put(ctx, model.ObjectKey, reader, input.SizeBytes, input.ContentType)
		closeErr := reader.Close()
		if err != nil {
			return Artifact{}, problem.Wrap(503, "artifact_promotion_failed", "Failed to promote the verified artifact object.", err)
		}
		if closeErr != nil {
			_ = s.store.Delete(ctx, model.ObjectKey)
			return Artifact{}, problem.Wrap(503, "artifact_promotion_failed", "Failed to close the verified upload object.", closeErr)
		}
		if _, err := s.verifyObject(ctx, model.ObjectKey, input, nil); err != nil {
			_ = s.store.Delete(ctx, model.ObjectKey)
			return Artifact{}, problem.Wrap(503, "artifact_promotion_verification_failed", "The promoted artifact object failed verification.", err)
		}
	}
	promoted := uploadKey != model.ObjectKey

	var appended persistence.SessionEvent
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ? AND deleted_at IS NULL", model.TenantID).Take(&tenant).Error; err != nil {
			return problem.Wrap(404, "tenant_not_found", "Tenant not found.", err)
		}
		var quota persistence.TenantQuota
		quotaErr := tx.WithContext(ctx).Where("tenant_id = ?", model.TenantID).Take(&quota).Error
		if quotaErr == nil && quota.MaxArtifactBytes != nil {
			var usedBytes int64
			if err := tx.WithContext(ctx).Model(&persistence.Artifact{}).
				Select("COALESCE(SUM(size_bytes), 0)").
				Where("tenant_id = ? AND status = ? AND deleted_at IS NULL", model.TenantID, "ready").
				Scan(&usedBytes).Error; err != nil {
				return problem.Wrap(500, "artifact_quota_check_failed", "Failed to check tenant artifact quota.", err)
			}
			if input.SizeBytes > *quota.MaxArtifactBytes || usedBytes > *quota.MaxArtifactBytes-input.SizeBytes {
				return problem.New(409, "artifact_quota_exceeded", "The tenant Artifact storage quota has been reached.")
			}
		} else if quotaErr != nil && !errors.Is(quotaErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "artifact_quota_check_failed", "Failed to load tenant artifact quota.", quotaErr)
		}
		var current persistence.Artifact
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ? AND tenant_id = ?", model.ID, model.TenantID).Take(&current).Error; err != nil {
			return problem.Wrap(404, "artifact_not_found", "Artifact not found.", err)
		}
		if workerLease != nil {
			if _, err := s.executions.AuthorizeArtifactWrite(ctx, tx, workerLease.worker, workerLease.executionID, workerLease.input); err != nil {
				return err
			}
		}
		if current.Status == "ready" {
			return nil
		}
		if current.Status != "pending" {
			return problem.New(409, "artifact_not_pending", "Artifact is not pending upload confirmation.")
		}
		now := s.now()
		updates := map[string]any{
			"status": "ready", "content_type": input.ContentType, "size_bytes": input.SizeBytes,
			"sha256": input.SHA256, "ready_at": now, "upload_token_hash": nil,
		}
		if s.store.IsLocal() {
			updates["upload_expires_at"] = nil
		}
		if info.Version != "" {
			updates["object_version"] = info.Version
		}
		result := tx.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND tenant_id = ? AND status = ?", current.ID, current.TenantID, "pending").Updates(updates)
		if result.Error != nil || result.RowsAffected != 1 {
			return problem.Wrap(409, "artifact_complete_conflict", "Artifact confirmation conflicted with another request.", result.Error)
		}
		actorID := current.CreatedByID
		eventInput := sessions.InternalEventInput{
			EventType: "artifact.ready", ActorType: current.CreatedByType, ActorID: &actorID,
			ExecutionID: current.ExecutionID,
			Payload:     map[string]any{"artifactId": current.ID, "kind": current.Kind, "sizeBytes": input.SizeBytes, "contentType": input.ContentType},
		}
		if workerLease != nil {
			generation := workerLease.input.Generation
			eventInput.WorkerID = &workerLease.worker.ID
			eventInput.Generation = &generation
		}
		appended, err = s.sessions.AppendInternalEvent(ctx, tx, current.TenantID, current.SessionID, eventInput)
		if err != nil {
			return err
		}
		tenantID := current.TenantID
		if err := outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
			TenantID: &tenantID, Topic: "artifact.ready", MessageKey: current.ID.String(),
			Payload: map[string]any{
				"tenantId": current.TenantID, "organizationId": current.OrganizationID,
				"projectId": current.ProjectID, "sessionId": current.SessionID,
				"executionId": current.ExecutionID, "artifactId": current.ID,
				"kind": current.Kind, "sizeBytes": input.SizeBytes, "contentType": input.ContentType,
			},
		}); err != nil {
			return problem.Wrap(500, "artifact_ready_outbox_failed", "The ready Artifact event could not be queued.", err)
		}
		if requestID != "" && userActorID != nil {
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: current.TenantID, ActorType: "user", ActorID: userActorID,
				Action: "artifact.ready", ResourceType: "artifact", ResourceID: &current.ID,
				OrganizationID: &current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			})
		}
		return nil
	})
	if err != nil {
		if promoted {
			_ = s.store.Delete(ctx, model.ObjectKey)
		}
		return Artifact{}, err
	}
	if appended.EventID != uuid.Nil {
		s.sessions.PublishInternalEvent(appended)
	}
	s.cleanupPromotedUpload(ctx, model)
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", model.ID, model.TenantID).Take(&model).Error; err != nil {
		return Artifact{}, problem.Wrap(500, "artifact_reload_failed", "Failed to reload the completed artifact.", err)
	}
	return toArtifact(model), nil
}

func (s *Service) List(ctx context.Context, principal identity.Principal, sessionID uuid.UUID) ([]Artifact, error) {
	session, err := s.authorizeSession(ctx, principal, sessionID, authorization.ArtifactRead, false)
	if err != nil {
		return nil, err
	}
	models := make([]persistence.Artifact, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND deleted_at IS NULL", session.TenantID, session.ID).
		Order("created_at DESC, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "artifacts_load_failed", "Failed to load artifacts.", err)
	}
	items := make([]Artifact, 0, len(models))
	for _, model := range models {
		items = append(items, toArtifact(model))
	}
	return items, nil
}

func (s *Service) Get(ctx context.Context, principal identity.Principal, artifactID uuid.UUID) (Artifact, error) {
	model, err := s.authorizedArtifact(ctx, principal, artifactID, authorization.ArtifactRead)
	if err != nil {
		return Artifact{}, err
	}
	return toArtifact(model), nil
}

func (s *Service) Download(ctx context.Context, principal identity.Principal, artifactID uuid.UUID) (DownloadGrant, error) {
	model, err := s.authorizedArtifact(ctx, principal, artifactID, authorization.ArtifactRead)
	if err != nil {
		return DownloadGrant{}, err
	}
	if model.Status != "ready" || model.DeletedAt != nil {
		return DownloadGrant{}, problem.New(409, "artifact_not_ready", "Artifact is not available for download.")
	}
	expiresAt := s.now().Add(s.presignTTL)
	grant := DownloadGrant{Artifact: toArtifact(model), ExpiresAt: expiresAt}
	if !s.store.IsLocal() {
		grant.URL, err = s.store.PresignDownload(ctx, model.ObjectKey, s.presignTTL)
		if err != nil {
			return DownloadGrant{}, problem.Wrap(503, "artifact_download_signing_failed", "Failed to issue an artifact download URL.", err)
		}
		return grant, nil
	}
	plain, hash, err := secret.NewToken()
	if err != nil {
		return DownloadGrant{}, problem.Wrap(500, "artifact_download_token_failed", "Failed to issue an artifact download token.", err)
	}
	_ = s.db.WithContext(ctx).Where("expires_at <= ?", s.now()).Delete(&persistence.ArtifactAccessToken{}).Error
	if err := s.db.WithContext(ctx).Create(&persistence.ArtifactAccessToken{
		ID: uuid.New(), ArtifactID: model.ID, TokenHash: hash, Purpose: "download", ExpiresAt: expiresAt,
	}).Error; err != nil {
		return DownloadGrant{}, problem.Wrap(500, "artifact_download_token_failed", "Failed to persist the artifact download token.", err)
	}
	grant.URL = "/v1/artifact-content/" + model.ID.String() + "?token=" + url.QueryEscape(plain)
	return grant, nil
}

func (s *Service) OpenDownload(ctx context.Context, artifactID uuid.UUID, plainToken string) (Artifact, io.ReadCloser, error) {
	if !s.store.IsLocal() {
		return Artifact{}, nil, problem.New(404, "not_found", "Route not found.")
	}
	var model persistence.Artifact
	err := s.db.WithContext(ctx).Table("artifacts AS a").Select("a.*").
		Joins("JOIN artifact_access_tokens AS t ON t.artifact_id = a.id").
		Where("a.id = ? AND a.status = ? AND a.deleted_at IS NULL", artifactID, "ready").
		Where("t.purpose = ? AND t.token_hash = ? AND t.expires_at > ?", "download", secret.HashToken(strings.TrimSpace(plainToken)), s.now()).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Artifact{}, nil, problem.New(401, "invalid_artifact_download_token", "The artifact download token is invalid or expired.")
	}
	if err != nil {
		return Artifact{}, nil, problem.Wrap(500, "artifact_download_lookup_failed", "Failed to authorize the artifact download.", err)
	}
	reader, err := s.store.Open(ctx, model.ObjectKey)
	if err != nil {
		return Artifact{}, nil, problem.Wrap(404, "artifact_object_missing", "The artifact payload is unavailable.", err)
	}
	return toArtifact(model), reader, nil
}

func (s *Service) Delete(
	ctx context.Context,
	principal identity.Principal,
	artifactID uuid.UUID,
	requestID, ipAddress string,
) error {
	model, err := s.authorizedArtifact(ctx, principal, artifactID, authorization.ArtifactDelete)
	if err != nil {
		return err
	}
	if model.Status == "deleted" {
		return nil
	}
	actorID := principal.UserID
	_, err = s.deleteModel(ctx, model, artifactDeleteActor{
		ActorType: "user", ActorID: &actorID, RequestID: requestID, IPAddress: ipAddress,
	})
	return err
}

type artifactDeleteActor struct {
	ActorType string
	ActorID   *uuid.UUID
	RequestID string
	IPAddress string
	Metadata  map[string]any
}

func (s *Service) deleteModel(
	ctx context.Context,
	model persistence.Artifact,
	actor artifactDeleteActor,
) (bool, error) {
	if model.Status == "deleted" {
		return false, nil
	}
	result := s.db.WithContext(ctx).Model(&persistence.Artifact{}).
		Where("id = ? AND tenant_id = ? AND status IN ?", model.ID, model.TenantID, []string{"pending", "ready", "failed", "deleting"}).
		Update("status", "deleting")
	if result.Error != nil || result.RowsAffected != 1 {
		return false, problem.Wrap(409, "artifact_delete_conflict", "Artifact deletion conflicted with another request.", result.Error)
	}
	if err := s.store.Delete(ctx, model.ObjectKey); err != nil {
		return false, problem.Wrap(503, "artifact_delete_failed", "Failed to delete the artifact payload; retry the deletion.", err)
	}
	if model.UploadObjectKey != nil {
		if err := s.store.Delete(ctx, *model.UploadObjectKey); err != nil {
			return false, problem.Wrap(503, "artifact_delete_failed", "Failed to delete the pending artifact payload; retry the deletion.", err)
		}
	}
	now := s.now()
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		update := tx.WithContext(ctx).Model(&persistence.Artifact{}).
			Where("id = ? AND tenant_id = ? AND status = ?", model.ID, model.TenantID, "deleting").
			Updates(map[string]any{
				"status": "deleted", "deleted_at": now, "upload_token_hash": nil,
				"upload_expires_at": nil, "upload_object_key": nil,
			})
		if update.Error != nil || update.RowsAffected != 1 {
			return problem.Wrap(409, "artifact_delete_commit_failed", "Failed to commit artifact deletion.", update.Error)
		}
		if err := tx.WithContext(ctx).Where("artifact_id = ?", model.ID).Delete(&persistence.ArtifactAccessToken{}).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: model.TenantID, ActorType: actor.ActorType, ActorID: actor.ActorID,
			Action: "artifact.deleted", ResourceType: "artifact", ResourceID: &model.ID,
			OrganizationID: &model.OrganizationID, RequestID: actor.RequestID, IPAddress: actor.IPAddress,
			Metadata: actor.Metadata,
		})
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) authorizedArtifact(ctx context.Context, principal identity.Principal, artifactID uuid.UUID, permission authorization.Permission) (persistence.Artifact, error) {
	tenantID, err := activeTenant(principal)
	if err != nil {
		return persistence.Artifact{}, err
	}
	var model persistence.Artifact
	err = s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, artifactID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.Artifact{}, problem.New(404, "artifact_not_found", "Artifact not found.")
	}
	if err != nil {
		return persistence.Artifact{}, problem.Wrap(500, "artifact_load_failed", "Failed to load the artifact.", err)
	}
	if _, err := s.authorizeSession(ctx, principal, model.SessionID, permission, permission != authorization.ArtifactRead); err != nil {
		return persistence.Artifact{}, err
	}
	return model, nil
}

func (s *Service) authorizeSession(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
	permission authorization.Permission,
	requireActive bool,
) (persistence.AgentSession, error) {
	tenantID, err := activeTenant(principal)
	if err != nil {
		return persistence.AgentSession{}, err
	}
	var model persistence.AgentSession
	query := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, sessionID)
	if requireActive {
		query = query.Where("status = ? AND archived_at IS NULL", "active")
	}
	if err := query.Take(&model).Error; err != nil {
		return persistence.AgentSession{}, problem.New(404, "session_not_found", "Session not found.")
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, model.OrganizationID, permission)
	if err != nil {
		return persistence.AgentSession{}, err
	}
	if model.Visibility == "private" && model.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, permission) {
		return persistence.AgentSession{}, problem.New(404, "session_not_found", "Session not found.")
	}
	return model, nil
}

func (s *Service) requireExecution(ctx context.Context, tenantID, sessionID, executionID uuid.UUID) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND session_id = ? AND id = ?", tenantID, sessionID, executionID).Count(&count).Error; err != nil {
		return problem.Wrap(500, "execution_load_failed", "Failed to validate the artifact execution.", err)
	}
	if count != 1 {
		return problem.New(404, "execution_not_found", "Execution not found.")
	}
	return nil
}

func (s *Service) normalizeCreate(input CreateInput) (CreateInput, error) {
	input.Kind = strings.ToLower(strings.TrimSpace(input.Kind))
	if _, ok := allowedKinds[input.Kind]; !ok {
		return CreateInput{}, problem.New(400, "invalid_artifact_kind", "Artifact kind must be attachment, generated_file, terminal_log, workspace_snapshot, or checkpoint.")
	}
	if input.OriginalName != nil {
		name := strings.TrimSpace(*input.OriginalName)
		if name == "" || len(name) > 512 || strings.ContainsAny(name, "\r\n\x00") {
			return CreateInput{}, problem.New(400, "invalid_artifact_name", "Artifact originalName is invalid.")
		}
		for _, character := range name {
			if unicode.IsControl(character) {
				return CreateInput{}, problem.New(400, "invalid_artifact_name", "Artifact originalName is invalid.")
			}
		}
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		input.OriginalName = &name
	}
	if input.ExecutionID != nil && *input.ExecutionID == uuid.Nil {
		return CreateInput{}, problem.New(400, "invalid_execution_id", "executionId is invalid.")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(s.now()) {
		return CreateInput{}, problem.New(400, "invalid_artifact_expiry", "expiresAt must be in the future.")
	}
	return input, nil
}

func (s *Service) pendingModel(session persistence.AgentSession, input CreateInput, actorType string, actorID uuid.UUID) (persistence.Artifact, string, error) {
	id := uuid.New()
	executionSegment := "_session"
	if input.ExecutionID != nil {
		executionSegment = input.ExecutionID.String()
	}
	objectKey := fmt.Sprintf(
		"tenants/%s/organizations/%s/projects/%s/sessions/%s/executions/%s/artifacts/%s",
		session.TenantID, session.OrganizationID, session.ProjectID, session.ID, executionSegment, id,
	)
	now := s.now()
	uploadExpiry := now.Add(s.presignTTL)
	model := persistence.Artifact{
		ID: id, TenantID: session.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: session.ID, ExecutionID: input.ExecutionID,
		Kind: input.Kind, Status: "pending", OriginalName: input.OriginalName,
		Bucket: s.store.Bucket(), ObjectKey: objectKey, CreatedByType: actorType,
		CreatedByID: actorID, UploadExpiresAt: &uploadExpiry, ExpiresAt: input.ExpiresAt,
	}
	if !s.store.IsLocal() {
		uploadObjectKey := objectKey + ".pending." + uuid.NewString()
		model.UploadObjectKey = &uploadObjectKey
		return model, "", nil
	}
	plain, hash, err := secret.NewToken()
	if err != nil {
		return persistence.Artifact{}, "", problem.Wrap(500, "artifact_upload_token_failed", "Failed to issue an artifact upload token.", err)
	}
	model.UploadTokenHash = hash
	return model, plain, nil
}

func (s *Service) verifyObject(ctx context.Context, objectKey string, input CompleteInput, localContentType *string) (ObjectInfo, error) {
	info, err := s.store.Stat(ctx, objectKey)
	if err != nil {
		return ObjectInfo{}, problem.Wrap(409, "artifact_object_missing", "The uploaded artifact object is unavailable.", err)
	}
	actualContentType := info.ContentType
	if s.store.IsLocal() && localContentType != nil {
		actualContentType = *localContentType
	}
	actualContentType, err = normalizeContentType(actualContentType)
	if err != nil {
		return ObjectInfo{}, problem.New(409, "artifact_content_type_mismatch", "The stored artifact Content-Type is invalid.")
	}
	if info.Size != input.SizeBytes || actualContentType != input.ContentType {
		return ObjectInfo{}, problem.New(409, "artifact_metadata_mismatch", "The uploaded artifact size or Content-Type does not match the submitted metadata.")
	}
	reader, err := s.store.Open(ctx, objectKey)
	if err != nil {
		return ObjectInfo{}, problem.Wrap(409, "artifact_object_unreadable", "The uploaded artifact object cannot be verified.", err)
	}
	hash := sha256.New()
	written, copyErr := copyContext(ctx, hash, io.LimitReader(reader, s.maxUploadBytes+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return ObjectInfo{}, problem.Wrap(409, "artifact_hash_failed", "Failed to verify the uploaded artifact.", copyErr)
	}
	if closeErr != nil {
		return ObjectInfo{}, problem.Wrap(409, "artifact_hash_failed", "Failed to close the uploaded artifact during verification.", closeErr)
	}
	if written != input.SizeBytes || written > s.maxUploadBytes || hex.EncodeToString(hash.Sum(nil)) != input.SHA256 {
		return ObjectInfo{}, problem.New(409, "artifact_hash_mismatch", "The uploaded artifact SHA-256 or size does not match the submitted metadata.")
	}
	return info, nil
}

func (s *Service) cleanupPromotedUpload(ctx context.Context, model persistence.Artifact) {
	if model.UploadObjectKey == nil || *model.UploadObjectKey == model.ObjectKey {
		return
	}
	if err := s.store.Delete(ctx, *model.UploadObjectKey); err != nil {
		return
	}
}

func normalizeComplete(input CompleteInput, maximum int64) (CompleteInput, error) {
	if input.SizeBytes < 0 || input.SizeBytes > maximum {
		return CompleteInput{}, problem.New(400, "invalid_artifact_size", "Artifact sizeBytes is outside the configured limit.")
	}
	input.SHA256 = strings.ToLower(strings.TrimSpace(input.SHA256))
	decoded, err := hex.DecodeString(input.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return CompleteInput{}, problem.New(400, "invalid_artifact_sha256", "Artifact sha256 must be 64 lowercase hexadecimal characters.")
	}
	input.ContentType, err = normalizeContentType(input.ContentType)
	if err != nil {
		return CompleteInput{}, err
	}
	return input, nil
}

func normalizeContentType(value string) (string, error) {
	value = strings.TrimSpace(value)
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || len(mediaType) > 200 {
		return "", problem.New(400, "invalid_artifact_content_type", "Artifact Content-Type is invalid.")
	}
	normalized := mime.FormatMediaType(strings.ToLower(mediaType), parameters)
	if normalized == "" || len(normalized) > 255 {
		return "", problem.New(400, "invalid_artifact_content_type", "Artifact Content-Type is invalid.")
	}
	return normalized, nil
}

func activeTenant(principal identity.Principal) (uuid.UUID, error) {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID == uuid.Nil {
		return uuid.Nil, problem.New(409, "active_tenant_required", "Select an active tenant before accessing artifacts.")
	}
	return *principal.ActiveTenantID, nil
}

func toArtifact(model persistence.Artifact) Artifact {
	return Artifact{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		ProjectID: model.ProjectID, SessionID: model.SessionID, ExecutionID: model.ExecutionID,
		Kind: model.Kind, Status: model.Status, OriginalName: model.OriginalName,
		ContentType: model.ContentType, SizeBytes: model.SizeBytes, SHA256: model.SHA256,
		CreatedByType: model.CreatedByType, CreatedByID: model.CreatedByID,
		ReadyAt: model.ReadyAt, CreatedAt: model.CreatedAt, ExpiresAt: model.ExpiresAt, DeletedAt: model.DeletedAt,
	}
}
