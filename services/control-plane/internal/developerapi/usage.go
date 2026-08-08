package developerapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const aggregateRoutePattern = "*"

type Admission struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAt    time.Time
	ResetAfter time.Duration
}

type UsageService struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	now        func() time.Time
}

type UsageSummary struct {
	RoutePattern     string `json:"routePattern"`
	AdmittedCount    int64  `json:"admittedCount"`
	RateLimitedCount int64  `json:"rateLimitedCount"`
	RequestCount     int64  `json:"requestCount"`
	SuccessCount     int64  `json:"successCount"`
	ClientErrorCount int64  `json:"clientErrorCount"`
	ServerErrorCount int64  `json:"serverErrorCount"`
	TotalDurationMS  int64  `json:"totalDurationMs"`
}

type UsageReport struct {
	TenantID         uuid.UUID      `json:"tenantId"`
	ServiceAccountID uuid.UUID      `json:"serviceAccountId"`
	OrganizationID   *uuid.UUID     `json:"organizationId"`
	From             time.Time      `json:"from"`
	To               time.Time      `json:"to"`
	Items            []UsageSummary `json:"items"`
}

type Option func(*UsageService)

func WithNow(now func() time.Time) Option {
	return func(service *UsageService) {
		if now != nil {
			service.now = now
		}
	}
}

func NewUsageService(db *gorm.DB, options ...Option) *UsageService {
	service := &UsageService{
		db: db, authorizer: authorization.NewAuthorizer(db),
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

func (s *UsageService) Summarize(
	ctx context.Context,
	principal identity.Principal,
	tenantID, serviceAccountID uuid.UUID,
	from, to *time.Time,
) (UsageReport, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return UsageReport{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.ServiceAccountsRead); err != nil {
		return UsageReport{}, err
	}
	now := s.now().UTC()
	toValue := now
	if to != nil {
		toValue = to.UTC()
	}
	fromValue := toValue.Add(-24 * time.Hour)
	if from != nil {
		fromValue = from.UTC()
	}
	if !fromValue.Before(toValue) || toValue.Sub(fromValue) > 31*24*time.Hour {
		return UsageReport{}, problem.New(400, "invalid_developer_api_usage_range", "Developer API usage range must be positive and no longer than 31 days.")
	}
	var account persistence.ServiceAccount
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, serviceAccountID).Take(&account).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return UsageReport{}, problem.New(404, "service_account_not_found", "Service Account not found.")
	} else if err != nil {
		return UsageReport{}, problem.Wrap(500, "developer_api_usage_load_failed", "Developer API usage could not be loaded.", err)
	}
	items := make([]UsageSummary, 0)
	err := s.db.WithContext(ctx).Model(&persistence.ServiceAccountAPIUsageWindow{}).
		Select(`route_pattern,
			SUM(admitted_count) AS admitted_count,
			SUM(rate_limited_count) AS rate_limited_count,
			SUM(request_count) AS request_count,
			SUM(success_count) AS success_count,
			SUM(client_error_count) AS client_error_count,
			SUM(server_error_count) AS server_error_count,
			SUM(total_duration_ms) AS total_duration_ms`).
		Where(
			"tenant_id = ? AND service_account_id = ? AND window_started_at >= ? AND window_started_at < ?",
			tenantID, serviceAccountID, fromValue, toValue,
		).
		Group("route_pattern").Order("route_pattern").Scan(&items).Error
	if err != nil {
		return UsageReport{}, problem.Wrap(500, "developer_api_usage_load_failed", "Developer API usage could not be loaded.", err)
	}
	return UsageReport{
		TenantID: tenantID, ServiceAccountID: serviceAccountID, OrganizationID: account.OrganizationID,
		From: fromValue, To: toValue, Items: items,
	}, nil
}

func (s *UsageService) Admit(
	ctx context.Context,
	tenantID, serviceAccountID uuid.UUID,
	organizationID *uuid.UUID,
	routePattern string,
	limit int,
) (Admission, error) {
	if s == nil || s.db == nil {
		return Admission{}, fmt.Errorf("developer API usage service is unavailable")
	}
	if limit < 1 {
		return Admission{}, fmt.Errorf("developer API rate limit must be positive")
	}
	routePattern = normalizeRoutePattern(routePattern)
	now := s.now().UTC()
	windowStartedAt := now.Truncate(time.Minute)
	admission := Admission{
		Limit: limit, ResetAt: windowStartedAt.Add(time.Minute),
		ResetAfter: windowStartedAt.Add(time.Minute).Sub(now),
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		aggregate, err := lockUsageRow(ctx, tx, tenantID, serviceAccountID, organizationID, windowStartedAt, aggregateRoutePattern)
		if err != nil {
			return err
		}
		if aggregate.AdmittedCount >= int64(limit) {
			admission.Allowed = false
			admission.Remaining = 0
			if err := incrementAdmissionCounter(tx, aggregate, false, now); err != nil {
				return err
			}
			if routePattern != aggregateRoutePattern {
				route, err := lockUsageRow(ctx, tx, tenantID, serviceAccountID, organizationID, windowStartedAt, routePattern)
				if err != nil {
					return err
				}
				return incrementAdmissionCounter(tx, route, false, now)
			}
			return nil
		}

		admission.Allowed = true
		admission.Remaining = limit - int(aggregate.AdmittedCount) - 1
		if err := incrementAdmissionCounter(tx, aggregate, true, now); err != nil {
			return err
		}
		if routePattern != aggregateRoutePattern {
			route, err := lockUsageRow(ctx, tx, tenantID, serviceAccountID, organizationID, windowStartedAt, routePattern)
			if err != nil {
				return err
			}
			return incrementAdmissionCounter(tx, route, true, now)
		}
		return nil
	})
	if err != nil {
		return Admission{}, fmt.Errorf("admit developer API request: %w", err)
	}
	return admission, nil
}

func (s *UsageService) RecordOutcome(
	ctx context.Context,
	tenantID, serviceAccountID uuid.UUID,
	windowStartedAt time.Time,
	routePattern string,
	status int,
	duration time.Duration,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("developer API usage service is unavailable")
	}
	routePattern = normalizeRoutePattern(routePattern)
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	updates := map[string]any{
		"total_duration_ms": gorm.Expr("total_duration_ms + ?", durationMS),
		"updated_at":        s.now().UTC(),
	}
	switch {
	case status >= 500:
		updates["server_error_count"] = gorm.Expr("server_error_count + 1")
	case status >= 400:
		updates["client_error_count"] = gorm.Expr("client_error_count + 1")
	default:
		updates["success_count"] = gorm.Expr("success_count + 1")
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		patterns := []string{aggregateRoutePattern}
		if routePattern != aggregateRoutePattern {
			patterns = append(patterns, routePattern)
		}
		for _, pattern := range patterns {
			result := tx.Model(&persistence.ServiceAccountAPIUsageWindow{}).
				Where(
					"tenant_id = ? AND service_account_id = ? AND window_started_at = ? AND route_pattern = ?",
					tenantID, serviceAccountID, windowStartedAt.UTC(), pattern,
				).
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("developer API usage row %q was not admitted", pattern)
			}
		}
		return nil
	})
}

func lockUsageRow(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, serviceAccountID uuid.UUID,
	organizationID *uuid.UUID,
	windowStartedAt time.Time,
	routePattern string,
) (persistence.ServiceAccountAPIUsageWindow, error) {
	seed := persistence.ServiceAccountAPIUsageWindow{
		TenantID: tenantID, ServiceAccountID: serviceAccountID, OrganizationID: organizationID,
		WindowStartedAt: windowStartedAt, RoutePattern: routePattern,
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
		return persistence.ServiceAccountAPIUsageWindow{}, err
	}
	var row persistence.ServiceAccountAPIUsageWindow
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where(
			"tenant_id = ? AND service_account_id = ? AND window_started_at = ? AND route_pattern = ?",
			tenantID, serviceAccountID, windowStartedAt, routePattern,
		).
		Take(&row).Error
	return row, err
}

func incrementAdmissionCounter(tx *gorm.DB, row persistence.ServiceAccountAPIUsageWindow, allowed bool, now time.Time) error {
	updates := map[string]any{"updated_at": now.UTC()}
	if allowed {
		updates["admitted_count"] = gorm.Expr("admitted_count + 1")
		updates["request_count"] = gorm.Expr("request_count + 1")
	} else {
		updates["rate_limited_count"] = gorm.Expr("rate_limited_count + 1")
	}
	return tx.Model(&persistence.ServiceAccountAPIUsageWindow{}).
		Where(
			"tenant_id = ? AND service_account_id = ? AND window_started_at = ? AND route_pattern = ?",
			row.TenantID, row.ServiceAccountID, row.WindowStartedAt, row.RoutePattern,
		).
		Updates(updates).Error
}

func normalizeRoutePattern(routePattern string) string {
	routePattern = strings.TrimSpace(routePattern)
	if routePattern == "" {
		return "UNKNOWN"
	}
	if len(routePattern) > 500 {
		return routePattern[:500]
	}
	return routePattern
}
