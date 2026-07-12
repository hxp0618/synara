package tenancy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const auditExportBatchSize = 500

type auditCursor struct {
	Version    int       `json:"version"`
	TenantID   uuid.UUID `json:"tenantId"`
	FilterHash string    `json:"filterHash"`
	OccurredAt time.Time `json:"occurredAt"`
	EventID    uuid.UUID `json:"eventId"`
}

func (s *Service) ListAuditLogs(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	query AuditLogQuery,
) (AuditLogPage, error) {
	if _, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.AuditRead); err != nil {
		return AuditLogPage{}, err
	}
	normalized, cursor, err := normalizeAuditQuery(query, true)
	if err != nil {
		return AuditLogPage{}, err
	}
	filterHash := auditFilterHash(normalized)
	if cursor != nil && (cursor.Version != 1 || cursor.TenantID != tenantID || cursor.FilterHash != filterHash) {
		return AuditLogPage{}, problem.New(400, "invalid_audit_cursor", "Audit log cursor does not match this tenant and filter.")
	}
	limit := persistence.NormalizeLimit(normalized.Limit, 50, 200)
	models, err := s.loadAuditLogBatch(ctx, tenantID, normalized, cursor, limit+1)
	if err != nil {
		return AuditLogPage{}, err
	}
	page := AuditLogPage{Items: make([]AuditLogEntry, 0, min(len(models), limit))}
	for _, model := range models[:min(len(models), limit)] {
		page.Items = append(page.Items, toAuditLogEntry(model))
	}
	if len(models) > limit {
		last := models[limit-1]
		encoded := encodeAuditCursor(auditCursor{
			Version: 1, TenantID: tenantID, FilterHash: filterHash,
			OccurredAt: last.OccurredAt, EventID: last.EventID,
		})
		page.NextCursor = &encoded
	}
	return page, nil
}

func (s *Service) ExportAuditLogs(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	query AuditLogQuery,
	format, requestID, ipAddress string,
	yield func(AuditLogEntry) error,
) error {
	if _, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.AuditRead); err != nil {
		return err
	}
	normalized, _, err := normalizeAuditQuery(query, false)
	if err != nil {
		return err
	}
	metadata := map[string]any{
		"format": format, "action": normalized.Action, "actorType": normalized.ActorType,
		"resourceType": normalized.ResourceType, "organizationId": normalized.OrganizationID,
		"occurredAfter": normalized.OccurredAfter, "occurredBefore": normalized.OccurredBefore,
	}
	if err := audit.Record(ctx, s.db, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "audit.export_started", ResourceType: "audit_log_export", RequestID: requestID,
		IPAddress: ipAddress, Metadata: metadata,
	}); err != nil {
		return err
	}
	var cursor *auditCursor
	exportedRows := 0
	for {
		models, err := s.loadAuditLogBatch(ctx, tenantID, normalized, cursor, auditExportBatchSize)
		if err != nil {
			return err
		}
		for _, model := range models {
			if err := yield(toAuditLogEntry(model)); err != nil {
				return err
			}
			exportedRows++
		}
		if len(models) < auditExportBatchSize {
			break
		}
		last := models[len(models)-1]
		cursor = &auditCursor{OccurredAt: last.OccurredAt, EventID: last.EventID}
	}
	completedMetadata := make(map[string]any, len(metadata)+1)
	for key, value := range metadata {
		completedMetadata[key] = value
	}
	completedMetadata["rows"] = exportedRows
	return audit.Record(ctx, s.db, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "audit.export_completed", ResourceType: "audit_log_export", RequestID: requestID,
		IPAddress: ipAddress, Metadata: completedMetadata,
	})
}

func (s *Service) loadAuditLogBatch(
	ctx context.Context,
	tenantID uuid.UUID,
	query AuditLogQuery,
	cursor *auditCursor,
	limit int,
) ([]persistence.AuditLog, error) {
	models := make([]persistence.AuditLog, 0, limit)
	db := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if query.Action != "" {
		db = db.Where("action = ?", query.Action)
	}
	if query.ActorType != "" {
		db = db.Where("actor_type = ?", query.ActorType)
	}
	if query.ResourceType != "" {
		db = db.Where("resource_type = ?", query.ResourceType)
	}
	if query.OrganizationID != nil {
		db = db.Where("organization_id = ?", *query.OrganizationID)
	}
	if query.OccurredAfter != nil {
		db = db.Where("occurred_at >= ?", query.OccurredAfter.UTC())
	}
	if query.OccurredBefore != nil {
		db = db.Where("occurred_at < ?", query.OccurredBefore.UTC())
	}
	if cursor != nil {
		db = db.Where("occurred_at < ? OR (occurred_at = ? AND event_id < ?)", cursor.OccurredAt, cursor.OccurredAt, cursor.EventID)
	}
	if err := db.Order("occurred_at DESC, event_id DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "audit_logs_load_failed", "Failed to load audit logs.", err)
	}
	return models, nil
}

func normalizeAuditQuery(query AuditLogQuery, allowCursor bool) (AuditLogQuery, *auditCursor, error) {
	query.Action = strings.TrimSpace(query.Action)
	query.ActorType = strings.TrimSpace(query.ActorType)
	query.ResourceType = strings.TrimSpace(query.ResourceType)
	if len(query.Action) > 160 || len(query.ResourceType) > 120 {
		return AuditLogQuery{}, nil, problem.New(400, "invalid_audit_filter", "Audit action and resourceType filters are too long.")
	}
	if query.ActorType != "" {
		switch query.ActorType {
		case "user", "service_account", "worker", "system":
		default:
			return AuditLogQuery{}, nil, problem.New(400, "invalid_audit_actor_type", "actorType is invalid.")
		}
	}
	if query.OccurredAfter != nil && query.OccurredBefore != nil && !query.OccurredAfter.Before(*query.OccurredBefore) {
		return AuditLogQuery{}, nil, problem.New(400, "invalid_audit_time_range", "occurredAfter must be before occurredBefore.")
	}
	if !allowCursor || strings.TrimSpace(query.Cursor) == "" {
		query.Cursor = ""
		return query, nil, nil
	}
	cursor, err := decodeAuditCursor(query.Cursor)
	if err != nil {
		return AuditLogQuery{}, nil, problem.New(400, "invalid_audit_cursor", "Audit log cursor is invalid.")
	}
	return query, &cursor, nil
}

func encodeAuditCursor(cursor auditCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func auditFilterHash(query AuditLogQuery) string {
	encoded, _ := json.Marshal(struct {
		Action         string     `json:"action"`
		ActorType      string     `json:"actorType"`
		ResourceType   string     `json:"resourceType"`
		OrganizationID *uuid.UUID `json:"organizationId"`
		OccurredAfter  *time.Time `json:"occurredAfter"`
		OccurredBefore *time.Time `json:"occurredBefore"`
	}{
		Action: query.Action, ActorType: query.ActorType, ResourceType: query.ResourceType,
		OrganizationID: query.OrganizationID, OccurredAfter: query.OccurredAfter,
		OccurredBefore: query.OccurredBefore,
	})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func decodeAuditCursor(value string) (auditCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return auditCursor{}, err
	}
	var cursor auditCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != 1 || cursor.TenantID == uuid.Nil ||
		cursor.FilterHash == "" || cursor.EventID == uuid.Nil || cursor.OccurredAt.IsZero() {
		return auditCursor{}, errors.New("invalid cursor")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return auditCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func toAuditLogEntry(model persistence.AuditLog) AuditLogEntry {
	metadata := model.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return AuditLogEntry{
		EventID: model.EventID, TenantID: model.TenantID, ActorType: model.ActorType,
		ActorID: model.ActorID, Action: model.Action, ResourceType: model.ResourceType,
		ResourceID: model.ResourceID, OrganizationID: model.OrganizationID,
		RequestID: model.RequestID, Metadata: metadata, OccurredAt: model.OccurredAt,
	}
}
