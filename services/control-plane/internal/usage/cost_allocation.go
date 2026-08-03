package usage

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const unallocatedDimension = "unallocated"

var costDimensionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`)

type ProjectCostAllocation struct {
	TenantID       uuid.UUID `json:"tenantId"`
	ProjectID      uuid.UUID `json:"projectId"`
	ProjectName    string    `json:"projectName"`
	OrganizationID uuid.UUID `json:"organizationId"`
	CostCenterCode string    `json:"costCenterCode"`
	DepartmentCode string    `json:"departmentCode"`
	Version        int64     `json:"version"`
	UpdatedBy      uuid.UUID `json:"updatedBy"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type PutProjectCostAllocationInput struct {
	CostCenterCode  string `json:"costCenterCode"`
	DepartmentCode  string `json:"departmentCode"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

type InternalCostAllocationRow struct {
	ProjectCostAllocation
	InputTokens               int64            `json:"inputTokens"`
	CachedInputTokens         int64            `json:"cachedInputTokens"`
	OutputTokens              int64            `json:"outputTokens"`
	ReasoningTokens           int64            `json:"reasoningTokens"`
	TotalTokens               int64            `json:"totalTokens"`
	NetworkIngressBytes       int64            `json:"networkIngressBytes"`
	NetworkEgressBytes        int64            `json:"networkEgressBytes"`
	ExecutionSeconds          int64            `json:"executionSeconds"`
	ProviderCostReportedCount int64            `json:"providerCostReportedCount"`
	ProviderCostMissingCount  int64            `json:"providerCostMissingCount"`
	ProviderCostByCurrency    map[string]int64 `json:"providerCostByCurrency"`
	PlatformCostByCurrency    map[string]int64 `json:"platformCostByCurrency"`
	KnownCostByCurrency       map[string]int64 `json:"knownCostByCurrency"`
}

type InternalCostAllocationReport struct {
	TenantID                uuid.UUID                   `json:"tenantId"`
	PlanAssignmentVersion   int64                       `json:"entitlementProfileVersion"`
	PeriodStart             string                      `json:"periodStart"`
	PeriodEnd               string                      `json:"periodEnd"`
	Rows                    []InternalCostAllocationRow `json:"rows"`
	UnallocatedProjectCount int64                       `json:"unallocatedProjectCount"`
	ProviderCostByCurrency  map[string]int64            `json:"providerCostByCurrency"`
	PlatformCostByCurrency  map[string]int64            `json:"platformCostByCurrency"`
	KnownCostByCurrency     map[string]int64            `json:"knownCostByCurrency"`
}

func normalizeCostDimension(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if !costDimensionPattern.MatchString(value) {
		return "", problem.New(400, "invalid_cost_allocation_dimension", label+" must be 1-64 letters, numbers, dots, underscores, slashes, or hyphens and start with a letter or number.")
	}
	return value, nil
}

func (s *Service) PutProjectCostAllocation(
	ctx context.Context,
	principal identity.Principal,
	tenantID, projectID uuid.UUID,
	input PutProjectCostAllocationInput,
	requestID, ipAddress string,
) (ProjectCostAllocation, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return ProjectCostAllocation{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CostManage); err != nil {
		return ProjectCostAllocation{}, err
	}
	if input.ExpectedVersion < 0 {
		return ProjectCostAllocation{}, problem.New(400, "invalid_cost_allocation_version", "Expected version must be zero or greater.")
	}
	costCenter, err := normalizeCostDimension(input.CostCenterCode, "Cost center code")
	if err != nil {
		return ProjectCostAllocation{}, err
	}
	department, err := normalizeCostDimension(input.DepartmentCode, "Department code")
	if err != nil {
		return ProjectCostAllocation{}, err
	}
	now := s.now().UTC()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var project persistence.Project
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, projectID).Take(&project).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "project_not_found", "Project not found.")
		} else if err != nil {
			return problem.Wrap(500, "project_cost_allocation_project_load_failed", "Project could not be loaded for cost allocation.", err)
		}
		var current persistence.ProjectCostAllocation
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND project_id = ?", tenantID, projectID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return problem.New(409, "project_cost_allocation_version_conflict", "Project cost allocation changed before this update.")
			}
			current = persistence.ProjectCostAllocation{
				TenantID: tenantID, ProjectID: projectID, CostCenterCode: costCenter,
				DepartmentCode: department, Version: 1, UpdatedBy: principal.UserID,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&current).Error; err != nil {
				return problem.Wrap(409, "project_cost_allocation_create_rejected", "Project cost allocation could not be created.", err)
			}
		} else if loadErr != nil {
			return problem.Wrap(500, "project_cost_allocation_load_failed", "Project cost allocation could not be loaded.", loadErr)
		} else {
			if current.Version != input.ExpectedVersion {
				return problem.New(409, "project_cost_allocation_version_conflict", "Project cost allocation changed before this update.")
			}
			updatedAt := now
			if !updatedAt.After(current.UpdatedAt) {
				updatedAt = current.UpdatedAt.Add(time.Millisecond)
			}
			result := tx.Model(&persistence.ProjectCostAllocation{}).
				Where("tenant_id = ? AND project_id = ? AND version = ?", tenantID, projectID, input.ExpectedVersion).
				Updates(map[string]any{
					"cost_center_code": costCenter, "department_code": department,
					"version": input.ExpectedVersion + 1, "updated_by": principal.UserID, "updated_at": updatedAt,
				})
			if result.Error != nil {
				return problem.Wrap(409, "project_cost_allocation_update_rejected", "Project cost allocation could not be updated.", result.Error)
			}
			if result.RowsAffected != 1 {
				return problem.New(409, "project_cost_allocation_version_conflict", "Project cost allocation changed before this update.")
			}
			current.CostCenterCode, current.DepartmentCode = costCenter, department
			current.Version, current.UpdatedBy, current.UpdatedAt = input.ExpectedVersion+1, principal.UserID, updatedAt
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "internal_cost.project_allocation_assigned", ResourceType: "project", ResourceID: &projectID,
			OrganizationID: &project.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"costCenterCode": costCenter, "departmentCode": department, "version": current.Version},
		})
	})
	if err != nil {
		return ProjectCostAllocation{}, err
	}
	return s.getProjectCostAllocation(ctx, tenantID, projectID)
}

func (s *Service) getProjectCostAllocation(ctx context.Context, tenantID, projectID uuid.UUID) (ProjectCostAllocation, error) {
	type row struct {
		persistence.ProjectCostAllocation
		ProjectName    string    `gorm:"column:project_name"`
		OrganizationID uuid.UUID `gorm:"column:organization_id"`
	}
	var item row
	if err := s.db.WithContext(ctx).Table("project_cost_allocations AS allocation").
		Select("allocation.*, project.name AS project_name, project.organization_id").
		Joins("JOIN projects AS project ON project.tenant_id = allocation.tenant_id AND project.id = allocation.project_id").
		Where("allocation.tenant_id = ? AND allocation.project_id = ?", tenantID, projectID).Take(&item).Error; err != nil {
		return ProjectCostAllocation{}, problem.Wrap(500, "project_cost_allocation_load_failed", "Project cost allocation could not be loaded.", err)
	}
	return ProjectCostAllocation{
		TenantID: item.TenantID, ProjectID: item.ProjectID, ProjectName: item.ProjectName,
		OrganizationID: item.OrganizationID, CostCenterCode: item.CostCenterCode,
		DepartmentCode: item.DepartmentCode, Version: item.Version, UpdatedBy: item.UpdatedBy, UpdatedAt: item.UpdatedAt,
	}, nil
}

func (s *Service) GetInternalCostAllocationReport(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (InternalCostAllocationReport, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return InternalCostAllocationReport{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.QuotaRead); err != nil {
		return InternalCostAllocationReport{}, err
	}
	policy, err := loadUsagePeriodPolicy(ctx, s.db, tenantID)
	if err != nil {
		return InternalCostAllocationReport{}, err
	}
	periodStart, periodEnd := policy.Subscription.CurrentPeriodStart, policy.Subscription.CurrentPeriodEnd
	type usageRow struct {
		ProjectID             uuid.UUID  `gorm:"column:project_id"`
		ProjectName           string     `gorm:"column:project_name"`
		OrganizationID        uuid.UUID  `gorm:"column:organization_id"`
		CostCenterCode        string     `gorm:"column:cost_center_code"`
		DepartmentCode        string     `gorm:"column:department_code"`
		AllocationVersion     int64      `gorm:"column:allocation_version"`
		UpdatedBy             uuid.UUID  `gorm:"column:updated_by"`
		AllocationUpdatedAt   *time.Time `gorm:"column:allocation_updated_at"`
		InputTokens           int64      `gorm:"column:input_tokens"`
		CachedInputTokens     int64      `gorm:"column:cached_input_tokens"`
		OutputTokens          int64      `gorm:"column:output_tokens"`
		ReasoningTokens       int64      `gorm:"column:reasoning_tokens"`
		TotalTokens           int64      `gorm:"column:total_tokens"`
		NetworkIngressBytes   int64      `gorm:"column:network_ingress_bytes"`
		NetworkEgressBytes    int64      `gorm:"column:network_egress_bytes"`
		DurationMillis        int64      `gorm:"column:duration_millis"`
		ExecutionCount        int64      `gorm:"column:execution_count"`
		ProviderReportedCount int64      `gorm:"column:provider_reported_count"`
	}
	var usageRows []usageRow
	if err := s.db.WithContext(ctx).Table("execution_usage_summaries AS usage").
		Select(`session.project_id, project.name AS project_name, project.organization_id,
			COALESCE(allocation.cost_center_code, 'unallocated') AS cost_center_code,
			COALESCE(allocation.department_code, 'unallocated') AS department_code,
			COALESCE(allocation.version, 0) AS allocation_version,
			COALESCE(allocation.updated_by, '00000000-0000-0000-0000-000000000000') AS updated_by,
			allocation.updated_at AS allocation_updated_at,
			SUM(usage.input_tokens) AS input_tokens, SUM(usage.cached_input_tokens) AS cached_input_tokens,
			SUM(usage.output_tokens) AS output_tokens, SUM(usage.reasoning_tokens) AS reasoning_tokens,
			SUM(usage.total_tokens) AS total_tokens, SUM(usage.network_ingress_bytes) AS network_ingress_bytes,
			SUM(usage.network_egress_bytes) AS network_egress_bytes, SUM(usage.duration_millis) AS duration_millis,
			COUNT(*) AS execution_count,
			SUM(CASE WHEN usage.provider_cost_reported THEN 1 ELSE 0 END) AS provider_reported_count`).
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = usage.tenant_id AND execution.id = usage.execution_id").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = usage.tenant_id AND session.id = usage.session_id").
		Joins("JOIN projects AS project ON project.tenant_id = session.tenant_id AND project.id = session.project_id").
		Joins("LEFT JOIN project_cost_allocations AS allocation ON allocation.tenant_id = project.tenant_id AND allocation.project_id = project.id").
		Where("usage.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ?", tenantID, periodStart, periodEnd).
		Group("session.project_id, project.name, project.organization_id, allocation.cost_center_code, allocation.department_code, allocation.version, allocation.updated_by, allocation.updated_at").
		Order("project.name, session.project_id").Scan(&usageRows).Error; err != nil {
		return InternalCostAllocationReport{}, problem.Wrap(500, "internal_cost_allocation_usage_load_failed", "Project usage allocation could not be loaded.", err)
	}
	rows := make(map[uuid.UUID]*InternalCostAllocationRow, len(usageRows))
	for _, item := range usageRows {
		updatedAt := time.Time{}
		if item.AllocationUpdatedAt != nil {
			updatedAt = *item.AllocationUpdatedAt
		}
		rows[item.ProjectID] = &InternalCostAllocationRow{
			ProjectCostAllocation: ProjectCostAllocation{
				TenantID: tenantID, ProjectID: item.ProjectID, ProjectName: item.ProjectName, OrganizationID: item.OrganizationID,
				CostCenterCode: item.CostCenterCode, DepartmentCode: item.DepartmentCode, Version: item.AllocationVersion,
				UpdatedBy: item.UpdatedBy, UpdatedAt: updatedAt,
			},
			InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens,
			ReasoningTokens: item.ReasoningTokens, TotalTokens: item.TotalTokens,
			NetworkIngressBytes: item.NetworkIngressBytes, NetworkEgressBytes: item.NetworkEgressBytes,
			ExecutionSeconds: executionSeconds(item.DurationMillis), ProviderCostReportedCount: item.ProviderReportedCount,
			ProviderCostMissingCount: item.ExecutionCount - item.ProviderReportedCount,
			ProviderCostByCurrency:   map[string]int64{}, PlatformCostByCurrency: map[string]int64{}, KnownCostByCurrency: map[string]int64{},
		}
	}
	type costRow struct {
		ProjectID    uuid.UUID `gorm:"column:project_id"`
		CurrencyCode string    `gorm:"column:currency_code"`
		AmountMicros int64     `gorm:"column:amount_micros"`
	}
	var providerRows []costRow
	if err := s.db.WithContext(ctx).Table("execution_usage_summaries AS usage").
		Select("session.project_id, usage.currency_code, SUM(usage.provider_cost_micros) AS amount_micros").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = usage.tenant_id AND execution.id = usage.execution_id").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = usage.tenant_id AND session.id = usage.session_id").
		Where("usage.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ? AND usage.provider_cost_reported = ?", tenantID, periodStart, periodEnd, true).
		Group("session.project_id, usage.currency_code").Scan(&providerRows).Error; err != nil {
		return InternalCostAllocationReport{}, problem.Wrap(500, "internal_cost_allocation_provider_load_failed", "Provider cost allocation could not be loaded.", err)
	}
	for _, item := range providerRows {
		if row := rows[item.ProjectID]; row != nil {
			row.ProviderCostByCurrency[item.CurrencyCode] += item.AmountMicros
			row.KnownCostByCurrency[item.CurrencyCode] += item.AmountMicros
		}
	}
	var platformRows []costRow
	if err := s.db.WithContext(ctx).Table("billing_shared_estimated_charge_slices AS slice").
		Select(`session.project_id, tariff.currency_code,
			SUM(CASE WHEN actual_run.id IS NULL THEN slice.amount_micros ELSE actual_slice.amount_micros END) AS amount_micros`).
		Joins("JOIN worker_claim_facts AS claim ON claim.id = slice.claim_fact_id").
		Joins("JOIN agent_executions AS execution ON execution.tenant_id = claim.tenant_id AND execution.id = claim.execution_id").
		Joins("JOIN agent_sessions AS session ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id").
		Joins("JOIN billing_provider_tariffs AS tariff ON tariff.id = slice.tariff_id").
		Joins("LEFT JOIN billing_shared_actual_charge_slices AS actual_slice ON actual_slice.estimated_slice_id = slice.id").
		Joins("LEFT JOIN billing_shared_actual_allocation_lines AS actual_line ON actual_line.id = actual_slice.allocation_line_id").
		Joins("LEFT JOIN billing_shared_actual_allocation_runs AS actual_run ON actual_run.id = actual_line.run_id AND actual_run.state = ?", "sealed").
		Where("slice.tenant_id = ? AND execution.queued_at >= ? AND execution.queued_at < ?", tenantID, periodStart, periodEnd).
		Group("session.project_id, tariff.currency_code").Scan(&platformRows).Error; err != nil {
		return InternalCostAllocationReport{}, problem.Wrap(500, "internal_cost_allocation_platform_load_failed", "Platform cost allocation could not be loaded.", err)
	}
	for _, item := range platformRows {
		if row := rows[item.ProjectID]; row != nil {
			row.PlatformCostByCurrency[item.CurrencyCode] += item.AmountMicros
			row.KnownCostByCurrency[item.CurrencyCode] += item.AmountMicros
		}
	}
	items := make([]InternalCostAllocationRow, 0, len(rows))
	for _, item := range rows {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ProjectName == items[j].ProjectName {
			return items[i].ProjectID.String() < items[j].ProjectID.String()
		}
		return items[i].ProjectName < items[j].ProjectName
	})
	report := InternalCostAllocationReport{
		TenantID: tenantID, PlanAssignmentVersion: policy.Subscription.Version,
		PeriodStart: periodStart.UTC().Format(time.RFC3339Nano), PeriodEnd: periodEnd.UTC().Format(time.RFC3339Nano), Rows: items,
		ProviderCostByCurrency: map[string]int64{}, PlatformCostByCurrency: map[string]int64{}, KnownCostByCurrency: map[string]int64{},
	}
	for _, item := range items {
		if item.CostCenterCode == unallocatedDimension || item.DepartmentCode == unallocatedDimension {
			report.UnallocatedProjectCount++
		}
		for currency, amount := range item.ProviderCostByCurrency {
			report.ProviderCostByCurrency[currency] += amount
		}
		for currency, amount := range item.PlatformCostByCurrency {
			report.PlatformCostByCurrency[currency] += amount
		}
		for currency, amount := range item.KnownCostByCurrency {
			report.KnownCostByCurrency[currency] += amount
		}
	}
	return report, nil
}

func (s *Service) RecordInternalCostAllocationExport(
	ctx context.Context,
	principal identity.Principal,
	report InternalCostAllocationReport,
	requestID, ipAddress string,
) error {
	if err := identity.RequireActiveTenant(principal, report.TenantID); err != nil {
		return err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, report.TenantID, authorization.QuotaRead); err != nil {
		return err
	}
	return audit.Record(ctx, s.db, audit.Entry{
		TenantID: report.TenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "internal_cost.allocation_exported", ResourceType: "tenant", ResourceID: &report.TenantID,
		RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{
			"periodStart": report.PeriodStart, "periodEnd": report.PeriodEnd,
			"projectCount": len(report.Rows), "unallocatedProjectCount": report.UnallocatedProjectCount,
			"format": "csv-v1",
		},
	})
}
