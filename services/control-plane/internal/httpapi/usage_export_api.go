package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
)

const tenantUsageExportSchema = "json-v1"

func (s *Server) exportTenantUsageJSON(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	principal := mustPrincipal(r)
	result, err := s.usage.GetTenantUsage(r.Context(), principal, tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	payload, err := json.Marshal(result)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Keep the same newline convention as the JSON API and hash the exact bytes
	// that are sent to the internal accounting consumer.
	payload = append(bytes.Clone(payload), '\n')
	digest := sha256.Sum256(payload)
	if err := audit.Record(r.Context(), s.db, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "tenant.usage_exported", ResourceType: "tenant_usage_export", ResourceID: &tenantID,
		RequestID: requestID(r), IPAddress: clientIP(r), Metadata: map[string]any{
			"schemaVersion": tenantUsageExportSchema, "format": tenantUsageExportSchema,
			"periodStart": result.PeriodStart, "periodEnd": result.PeriodEnd,
			"entitlementProfileVersion": result.SubscriptionVersion,
			"providerCostReportedCount": result.Usage.ProviderCostReportedCount,
			"providerCostMissingCount":  result.Usage.ProviderCostMissingCount,
			"bytes":                     len(payload), "sha256": "sha256:" + hex.EncodeToString(digest[:]),
		},
	}); err != nil {
		s.writeError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="synara-tenant-usage-`+tenantID.String()+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Synara-Usage-Export-Schema", tenantUsageExportSchema)
	w.Header().Set("X-Synara-Usage-Export-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("X-Synara-Usage-Export-Bytes", strconv.Itoa(len(payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
