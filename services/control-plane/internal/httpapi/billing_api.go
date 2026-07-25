package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) listBillingTariffs(w http.ResponseWriter, r *http.Request) {
	service := s.requireBillingService(w, r)
	if service == nil {
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	filter, err := parseBillingTariffListQuery(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := service.ListTariffsAuthorized(r.Context(), mustPrincipal(r), tenantID, filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createBillingTariff(w http.ResponseWriter, r *http.Request) {
	service := s.requireBillingService(w, r)
	if service == nil {
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input billing.CreateTariffInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := service.CreateTariffAuthorized(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) triggerBillingImport(w http.ResponseWriter, r *http.Request) {
	service := s.requireBillingService(w, r)
	if service == nil {
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.PathValue("provider"))
	externalImportID := strings.TrimSpace(r.PathValue("externalImportID"))
	item, err := service.ImportConfiguredInvoiceAuthorized(
		r.Context(), mustPrincipal(r), tenantID, provider, externalImportID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) reconcileBillingImport(w http.ResponseWriter, r *http.Request) {
	service := s.requireBillingService(w, r)
	if service == nil {
		return
	}
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	importID, ok := s.pathUUID(w, r, "importID")
	if !ok {
		return
	}
	report, err := service.ReconcileActualInvoiceImportAuthorized(
		r.Context(), mustPrincipal(r), tenantID, importID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) requireBillingService(w http.ResponseWriter, r *http.Request) *billing.Service {
	if s.billing != nil {
		return s.billing
	}
	s.writeError(w, r, problem.New(503, "billing_unavailable", "Billing is not configured."))
	return nil
}

func parseBillingTariffListQuery(r *http.Request) (billing.ListTariffsFilter, error) {
	values := r.URL.Query()
	filter := billing.ListTariffsFilter{}
	if values.Has("provider") {
		value := values.Get("provider")
		filter.Provider = &value
	}
	if values.Has("region") {
		value := values.Get("region")
		filter.Region = &value
	}
	if values.Has("currencyCode") {
		value := values.Get("currencyCode")
		filter.CurrencyCode = &value
	}
	if raw := strings.TrimSpace(values.Get("effectiveAt")); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return billing.ListTariffsFilter{}, problem.New(400, "invalid_billing_tariff_time", "effectiveAt must use RFC3339.")
		}
		parsed = parsed.UTC()
		filter.EffectiveAt = &parsed
	}
	return filter, nil
}
