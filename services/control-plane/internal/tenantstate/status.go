package tenantstate

import "time"

func IsOperational(status string, trialExpiresAt *time.Time, now time.Time) bool {
	if status == "active" {
		return true
	}
	return status == "trialing" && trialExpiresAt != nil && trialExpiresAt.After(now)
}
