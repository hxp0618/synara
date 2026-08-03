package usage

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type PlatformCharge struct {
	Kind         string `json:"kind"`
	CurrencyCode string `json:"currencyCode"`
	AmountMicros int64  `json:"amountMicros"`
	Source       string `json:"source"`
}

type ExecutionUsage struct {
	ExecutionID          uuid.UUID        `json:"executionId"`
	Generation           int64            `json:"generation"`
	TurnID               uuid.UUID        `json:"turnId"`
	Provider             string           `json:"provider"`
	Model                *string          `json:"model"`
	InputTokens          int64            `json:"inputTokens"`
	CachedInputTokens    int64            `json:"cachedInputTokens"`
	OutputTokens         int64            `json:"outputTokens"`
	ReasoningTokens      int64            `json:"reasoningTokens"`
	TotalTokens          int64            `json:"totalTokens"`
	NetworkIngressBytes  int64            `json:"networkIngressBytes"`
	NetworkEgressBytes   int64            `json:"networkEgressBytes"`
	DurationMillis       int64            `json:"durationMillis"`
	ProviderCostMicros   int64            `json:"providerCostMicros"`
	ProviderCostReported bool             `json:"providerCostReported"`
	ProviderCurrency     string           `json:"providerCurrency"`
	PlatformCharges      []PlatformCharge `json:"platformCharges"`
	TotalCostByCurrency  map[string]int64 `json:"totalCostByCurrency"`
	CostCoverage         string           `json:"costCoverage"`
	Final                bool             `json:"final"`
	UpdatedAt            time.Time        `json:"updatedAt"`
}

type SessionUsage struct {
	TenantID  uuid.UUID        `json:"tenantId"`
	SessionID uuid.UUID        `json:"sessionId"`
	Items     []ExecutionUsage `json:"items"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) GetSessionUsage(
	ctx context.Context,
	principal identity.Principal,
	sessionID uuid.UUID,
) (SessionUsage, error) {
	if principal.ActiveTenantID == nil {
		return SessionUsage{}, problem.New(404, "session_not_found", "Session not found.")
	}
	tenantID := *principal.ActiveTenantID
	var session persistence.AgentSession
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, sessionID).Take(&session).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return SessionUsage{}, problem.New(404, "session_not_found", "Session not found.")
	} else if err != nil {
		return SessionUsage{}, problem.Wrap(500, "session_usage_load_failed", "Session Usage could not be loaded.", err)
	}
	access, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, session.OrganizationID, authorization.SessionRead)
	if err != nil {
		return SessionUsage{}, err
	}
	if session.Visibility == "private" && session.CreatedBy != principal.UserID && !authorization.TenantAllows(access.TenantRole, authorization.SessionRead) {
		return SessionUsage{}, problem.New(404, "session_not_found", "Session not found.")
	}
	var summaries []persistence.ExecutionUsageSummary
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		Order("updated_at, execution_id, generation").Find(&summaries).Error; err != nil {
		return SessionUsage{}, problem.Wrap(500, "session_usage_load_failed", "Session Usage could not be loaded.", err)
	}
	type chargeRow struct {
		ExecutionID  uuid.UUID `gorm:"column:execution_id"`
		Generation   int64     `gorm:"column:generation"`
		Kind         string    `gorm:"column:charge_kind"`
		CurrencyCode string    `gorm:"column:currency_code"`
		AmountMicros int64     `gorm:"column:amount_micros"`
		Source       string    `gorm:"column:source"`
	}
	var chargeRows []chargeRow
	if err := s.db.WithContext(ctx).Table("billing_shared_estimated_charge_slices AS slice").
		Select(`claim.execution_id, claim.execution_generation AS generation, slice.charge_kind,
			tariff.currency_code,
			CASE WHEN actual_run.id IS NULL THEN 'estimated' ELSE 'actual' END AS source,
			SUM(CASE WHEN actual_run.id IS NULL THEN slice.amount_micros ELSE actual_slice.amount_micros END) AS amount_micros`).
		Joins("JOIN worker_claim_facts AS claim ON claim.id = slice.claim_fact_id").
		Joins("JOIN billing_provider_tariffs AS tariff ON tariff.id = slice.tariff_id").
		Joins("JOIN execution_usage_summaries AS usage ON usage.tenant_id = claim.tenant_id AND usage.execution_id = claim.execution_id AND usage.generation = claim.execution_generation").
		Joins("LEFT JOIN billing_shared_actual_charge_slices AS actual_slice ON actual_slice.estimated_slice_id = slice.id").
		Joins("LEFT JOIN billing_shared_actual_allocation_lines AS actual_line ON actual_line.id = actual_slice.allocation_line_id").
		Joins("LEFT JOIN billing_shared_actual_allocation_runs AS actual_run ON actual_run.id = actual_line.run_id AND actual_run.state = ?", "sealed").
		Where("claim.tenant_id = ? AND usage.session_id = ?", tenantID, sessionID).
		Group("claim.execution_id, claim.execution_generation, slice.charge_kind, tariff.currency_code, CASE WHEN actual_run.id IS NULL THEN 'estimated' ELSE 'actual' END").
		Scan(&chargeRows).Error; err != nil {
		return SessionUsage{}, problem.Wrap(500, "session_platform_cost_load_failed", "Allocated platform cost could not be loaded.", err)
	}
	type usageKey struct {
		ExecutionID uuid.UUID
		Generation  int64
	}
	chargesByUsage := make(map[usageKey][]PlatformCharge)
	for _, row := range chargeRows {
		key := usageKey{ExecutionID: row.ExecutionID, Generation: row.Generation}
		chargesByUsage[key] = append(chargesByUsage[key], PlatformCharge{
			Kind: row.Kind, CurrencyCode: row.CurrencyCode, AmountMicros: row.AmountMicros, Source: row.Source,
		})
	}
	items := make([]ExecutionUsage, 0, len(summaries))
	for _, summary := range summaries {
		charges := chargesByUsage[usageKey{ExecutionID: summary.ExecutionID, Generation: summary.Generation}]
		sort.Slice(charges, func(i, j int) bool {
			if charges[i].CurrencyCode == charges[j].CurrencyCode {
				if charges[i].Kind == charges[j].Kind {
					return charges[i].Source < charges[j].Source
				}
				return charges[i].Kind < charges[j].Kind
			}
			return charges[i].CurrencyCode < charges[j].CurrencyCode
		})
		if charges == nil {
			charges = []PlatformCharge{}
		}
		totals := map[string]int64{}
		if summary.ProviderCostReported {
			totals[summary.CurrencyCode] = summary.ProviderCostMicros
		}
		for _, charge := range charges {
			totals[charge.CurrencyCode] += charge.AmountMicros
		}
		coverage := "provider-unavailable"
		if summary.ProviderCostReported {
			coverage = "provider-only"
		}
		if len(charges) > 0 && summary.ProviderCostReported {
			coverage = "provider-and-allocated-platform"
		} else if len(charges) > 0 {
			coverage = "provider-unavailable-with-allocated-platform"
		}
		items = append(items, ExecutionUsage{
			ExecutionID: summary.ExecutionID, Generation: summary.Generation, TurnID: summary.TurnID,
			Provider: summary.Provider, Model: summary.Model,
			InputTokens: summary.InputTokens, CachedInputTokens: summary.CachedInputTokens,
			OutputTokens: summary.OutputTokens, ReasoningTokens: summary.ReasoningTokens, TotalTokens: summary.TotalTokens,
			NetworkIngressBytes: summary.NetworkIngressBytes, NetworkEgressBytes: summary.NetworkEgressBytes,
			DurationMillis: summary.DurationMillis, ProviderCostMicros: summary.ProviderCostMicros,
			ProviderCostReported: summary.ProviderCostReported,
			ProviderCurrency:     summary.CurrencyCode, PlatformCharges: charges,
			TotalCostByCurrency: totals, CostCoverage: coverage, Final: summary.Final, UpdatedAt: summary.UpdatedAt,
		})
	}
	return SessionUsage{TenantID: tenantID, SessionID: sessionID, Items: items}, nil
}
