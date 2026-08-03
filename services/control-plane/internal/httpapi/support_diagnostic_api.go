package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/usage"
)

const tenantSupportDiagnosticSchema = "json-v1"

// tenantSupportDiagnostic is intentionally an aggregate-only support packet.
// It is safe for the read-only Support Access flow: no credential payloads,
// artifact contents, execution prompts, or database records are included.
type tenantSupportDiagnostic struct {
	SchemaVersion string                       `json:"schemaVersion"`
	GeneratedAt   time.Time                    `json:"generatedAt"`
	Tenant        supportaccess.TenantOverview `json:"tenant"`
	Usage         usage.TenantUsage            `json:"usage"`
}

func (s *Server) exportTenantSupportDiagnosticJSON(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	principal := mustPrincipal(r)
	overview, err := s.supportAccess.GetTenantOverview(r.Context(), principal, tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	currentUsage, err := s.usage.GetTenantUsage(r.Context(), principal, tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	result := tenantSupportDiagnostic{
		SchemaVersion: tenantSupportDiagnosticSchema,
		GeneratedAt:   time.Now().UTC(),
		Tenant:        overview,
		Usage:         currentUsage,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Hash and report the exact bytes sent to the internal support operator.
	payload = append(bytes.Clone(payload), '\n')
	digest := sha256.Sum256(payload)
	if err := audit.Record(r.Context(), s.db, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "tenant.support_diagnostic_exported", ResourceType: "tenant_support_diagnostic", ResourceID: &tenantID,
		RequestID: requestID(r), IPAddress: clientIP(r), Metadata: map[string]any{
			"schemaVersion":             tenantSupportDiagnosticSchema,
			"bytes":                     len(payload),
			"sha256":                    "sha256:" + hex.EncodeToString(digest[:]),
			"activeExecutionCount":      overview.ActiveExecutionCount,
			"queuedExecutionCount":      overview.QueuedExecutionCount,
			"providerCostReportedCount": currentUsage.Usage.ProviderCostReportedCount,
			"providerCostMissingCount":  currentUsage.Usage.ProviderCostMissingCount,
		},
	}); err != nil {
		s.writeError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="synara-support-diagnostic-`+tenantID.String()+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Synara-Support-Diagnostic-Schema", tenantSupportDiagnosticSchema)
	w.Header().Set("X-Synara-Support-Diagnostic-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("X-Synara-Support-Diagnostic-Bytes", strconv.Itoa(len(payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
