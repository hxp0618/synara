package productprofile

import "strings"

const (
	ProfileStandard   = "standard"
	ProfileEnterprise = "enterprise"
	StatusEvaluation  = "evaluation"
)

// PublicProfileCode maps retained persistence codes to the internal-self-hosted API vocabulary.
func PublicProfileCode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "free":
		return ProfileStandard
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// InternalProfileCode maps the current API vocabulary back to retained persistence codes.
func InternalProfileCode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ProfileStandard:
		return "free"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// PublicLifecycleStatus prevents the retained trialing state from becoming a product trial surface.
func PublicLifecycleStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "trialing":
		return StatusEvaluation
	case "cancelled":
		return "disabled"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// InternalLifecycleStatus accepts only the product vocabulary at current API boundaries.
func InternalLifecycleStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case StatusEvaluation:
		return "trialing"
	case "disabled":
		return "cancelled"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func PublicAssignmentSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "self_service":
		return "user"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}
