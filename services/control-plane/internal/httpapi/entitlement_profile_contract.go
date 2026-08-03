package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/entitlements"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
)

type entitlementProfileResponse struct {
	TenantID uuid.UUID `json:"tenantId"`
	Profile  struct {
		Code           string `json:"code"`
		DisplayName    string `json:"displayName"`
		Status         string `json:"status"`
		Version        int64  `json:"version"`
		EvaluationDays int    `json:"evaluationDays"`
	} `json:"profile"`
	Assignment struct {
		Status               string     `json:"status"`
		Version              int64      `json:"version"`
		EvaluationEndsAt     *time.Time `json:"evaluationEndsAt"`
		ReportingPeriodStart time.Time  `json:"reportingPeriodStart"`
		ReportingPeriodEnd   time.Time  `json:"reportingPeriodEnd"`
		AssignmentSource     string     `json:"assignmentSource"`
	} `json:"profileAssignment"`
	Entitlements map[string]entitlements.Value `json:"entitlements"`
	Features     map[string]bool               `json:"features"`
}

type assignEntitlementProfileRequest struct {
	ProfileCode          string     `json:"entitlementProfileCode"`
	Status               string     `json:"status"`
	ExpectedVersion      int64      `json:"expectedVersion"`
	EvaluationEndsAt     *time.Time `json:"evaluationEndsAt"`
	ReportingPeriodStart time.Time  `json:"reportingPeriodStart"`
	ReportingPeriodEnd   time.Time  `json:"reportingPeriodEnd"`
	Reason               string     `json:"reason"`
}

func entitlementProfileView(snapshot entitlements.Snapshot) entitlementProfileResponse {
	view := entitlementProfileResponse{
		TenantID: snapshot.TenantID, Entitlements: snapshot.Entitlements, Features: snapshot.Features,
	}
	view.Profile.Code = productprofile.PublicProfileCode(snapshot.Plan.Code)
	view.Profile.DisplayName = snapshot.Plan.DisplayName
	view.Profile.Status = snapshot.Plan.Status
	view.Profile.Version = snapshot.Plan.Version
	view.Profile.EvaluationDays = snapshot.Plan.TrialDays
	view.Assignment.Status = productprofile.PublicLifecycleStatus(snapshot.Subscription.Status)
	view.Assignment.Version = snapshot.Subscription.Version
	view.Assignment.EvaluationEndsAt = snapshot.Subscription.TrialEndsAt
	view.Assignment.ReportingPeriodStart = snapshot.Subscription.CurrentPeriodStart
	view.Assignment.ReportingPeriodEnd = snapshot.Subscription.CurrentPeriodEnd
	view.Assignment.AssignmentSource = productprofile.PublicAssignmentSource(snapshot.Subscription.AssignmentSource)
	return view
}

func (input assignEntitlementProfileRequest) internalInput() entitlements.AssignPlanInput {
	return entitlements.AssignPlanInput{
		PlanCode: productprofile.InternalProfileCode(input.ProfileCode),
		Status:   productprofile.InternalLifecycleStatus(input.Status), ExpectedVersion: input.ExpectedVersion,
		TrialEndsAt: input.EvaluationEndsAt, CurrentPeriodStart: input.ReportingPeriodStart,
		CurrentPeriodEnd: input.ReportingPeriodEnd, Reason: input.Reason,
	}
}
