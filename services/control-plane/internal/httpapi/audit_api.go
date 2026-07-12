package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
)

func (s *Server) listAuditLogs(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	query, err := parseAuditLogQuery(r, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := s.tenancy.ListAuditLogs(r.Context(), mustPrincipal(r), tenantID, query)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) exportAuditLogs(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	query, err := parseAuditLogQuery(r, false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "jsonl"
	}
	if format != "jsonl" && format != "csv" {
		s.writeError(w, r, problem.New(400, "invalid_audit_export_format", "Audit export format must be jsonl or csv."))
		return
	}
	extension := format
	contentType := "application/x-ndjson; charset=utf-8"
	if format == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="synara-audit-%s-%s.%s"`, tenantID, time.Now().UTC().Format("20060102T150405Z"), extension,
	))
	w.Header().Set("Cache-Control", "no-store")

	wroteBody := false
	if format == "jsonl" {
		encoder := json.NewEncoder(w)
		err = s.tenancy.ExportAuditLogs(
			r.Context(), mustPrincipal(r), tenantID, query, format, requestID(r), clientIP(r),
			func(entry tenancy.AuditLogEntry) error {
				if err := encoder.Encode(entry); err != nil {
					return err
				}
				wroteBody = true
				return nil
			},
		)
	} else {
		writer := csv.NewWriter(w)
		headerWritten := false
		writeHeader := func() error {
			if headerWritten {
				return nil
			}
			headerWritten = true
			return writer.Write([]string{
				"event_id", "occurred_at", "tenant_id", "organization_id", "actor_type", "actor_id",
				"action", "resource_type", "resource_id", "request_id", "metadata_json",
			})
		}
		err = s.tenancy.ExportAuditLogs(
			r.Context(), mustPrincipal(r), tenantID, query, format, requestID(r), clientIP(r),
			func(entry tenancy.AuditLogEntry) error {
				if err := writeHeader(); err != nil {
					return err
				}
				metadata, err := json.Marshal(entry.Metadata)
				if err != nil {
					return err
				}
				if err := writer.Write([]string{
					entry.EventID.String(), entry.OccurredAt.UTC().Format(time.RFC3339Nano), entry.TenantID.String(),
					optionalUUIDString(entry.OrganizationID), entry.ActorType, optionalUUIDString(entry.ActorID),
					entry.Action, entry.ResourceType, optionalUUIDString(entry.ResourceID), entry.RequestID, string(metadata),
				}); err != nil {
					return err
				}
				wroteBody = true
				return nil
			},
		)
		if err == nil {
			if headerErr := writeHeader(); headerErr != nil {
				err = headerErr
			}
		}
		writer.Flush()
		if err == nil {
			err = writer.Error()
		}
	}
	if err == nil {
		return
	}
	if !wroteBody {
		s.writeError(w, r, err)
		return
	}
	s.logger.Warn("audit export stream failed", "tenantId", tenantID, "format", format, "error", err)
}

func parseAuditLogQuery(r *http.Request, includePage bool) (tenancy.AuditLogQuery, error) {
	values := r.URL.Query()
	query := tenancy.AuditLogQuery{
		Cursor: values.Get("cursor"), Action: values.Get("action"), ActorType: values.Get("actorType"),
		ResourceType: values.Get("resourceType"),
	}
	if includePage {
		if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil || limit <= 0 {
				return tenancy.AuditLogQuery{}, problem.New(400, "invalid_audit_limit", "Audit log limit must be a positive integer.")
			}
			query.Limit = limit
		}
	}
	if raw := strings.TrimSpace(values.Get("organizationId")); raw != "" {
		organizationID, err := uuid.Parse(raw)
		if err != nil {
			return tenancy.AuditLogQuery{}, problem.New(400, "invalid_organization_id", "organizationId must be a UUID.")
		}
		query.OrganizationID = &organizationID
	}
	var err error
	if query.OccurredAfter, err = parseOptionalAuditTime(values.Get("occurredAfter")); err != nil {
		return tenancy.AuditLogQuery{}, err
	}
	if query.OccurredBefore, err = parseOptionalAuditTime(values.Get("occurredBefore")); err != nil {
		return tenancy.AuditLogQuery{}, err
	}
	return query, nil
}

func parseOptionalAuditTime(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, problem.New(400, "invalid_audit_time", "Audit time filters must use RFC3339.")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func optionalUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}
