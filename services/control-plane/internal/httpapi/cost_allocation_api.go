package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/usage"
)

func (s *Server) getInternalCostAllocationReport(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	report, err := s.usage.GetInternalCostAllocationReport(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) putProjectCostAllocation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	var input usage.PutProjectCostAllocationInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.usage.PutProjectCostAllocation(
		r.Context(), mustPrincipal(r), tenantID, projectID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) exportInternalCostAllocationCSV(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	principal := mustPrincipal(r)
	report, err := s.usage.GetInternalCostAllocationReport(r.Context(), principal, tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.usage.RecordInternalCostAllocationExport(r.Context(), principal, report, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{
		"row_type", "tenant_id", "period_start", "period_end", "project_id", "project_name", "organization_id",
		"cost_center_code", "department_code", "allocation_version", "currency", "input_tokens", "cached_input_tokens",
		"output_tokens", "reasoning_tokens", "total_tokens", "network_ingress_bytes", "network_egress_bytes",
		"execution_seconds", "provider_cost_reported_count", "provider_cost_missing_count",
		"provider_cost_micros", "platform_cost_micros", "known_cost_micros",
	})
	for _, row := range report.Rows {
		base := []string{
			tenantID.String(), report.PeriodStart, report.PeriodEnd, row.ProjectID.String(), row.ProjectName,
			row.OrganizationID.String(), row.CostCenterCode, row.DepartmentCode, strconv.FormatInt(row.Version, 10),
		}
		_ = writer.Write(append([]string{"usage"}, append(base, []string{
			"", strconv.FormatInt(row.InputTokens, 10), strconv.FormatInt(row.CachedInputTokens, 10),
			strconv.FormatInt(row.OutputTokens, 10), strconv.FormatInt(row.ReasoningTokens, 10), strconv.FormatInt(row.TotalTokens, 10),
			strconv.FormatInt(row.NetworkIngressBytes, 10), strconv.FormatInt(row.NetworkEgressBytes, 10),
			strconv.FormatInt(row.ExecutionSeconds, 10), strconv.FormatInt(row.ProviderCostReportedCount, 10),
			strconv.FormatInt(row.ProviderCostMissingCount, 10), "", "", "",
		}...)...))
		currencies := map[string]struct{}{}
		for currency := range row.KnownCostByCurrency {
			currencies[currency] = struct{}{}
		}
		for currency := range row.ProviderCostByCurrency {
			currencies[currency] = struct{}{}
		}
		for currency := range row.PlatformCostByCurrency {
			currencies[currency] = struct{}{}
		}
		ordered := make([]string, 0, len(currencies))
		for currency := range currencies {
			ordered = append(ordered, currency)
		}
		sort.Strings(ordered)
		for _, currency := range ordered {
			_ = writer.Write(append([]string{"cost"}, append(base, []string{
				currency, "", "", "", "", "", "", "", "", "", "",
				strconv.FormatInt(row.ProviderCostByCurrency[currency], 10),
				strconv.FormatInt(row.PlatformCostByCurrency[currency], 10),
				strconv.FormatInt(row.KnownCostByCurrency[currency], 10),
			}...)...))
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		s.writeError(w, r, fmt.Errorf("internal cost CSV export failed: %w", err))
		return
	}
	payload := buffer.Bytes()
	digest := sha256.Sum256(payload)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="synara-internal-cost-%s-%s.csv"`, tenantID, time.Now().UTC().Format("20060102T150405Z"),
	))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Synara-Cost-Export-Schema", "csv-v1")
	w.Header().Set("X-Synara-Cost-Export-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("X-Synara-Cost-Export-Bytes", strconv.Itoa(len(payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	_, _ = w.Write(payload)
}
