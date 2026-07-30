package executiontargets

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const runtimeIsolationObservationTTL = 45 * time.Second

func persistRuntimeIsolationObservation(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
	capabilities []runtimeIsolationCapability,
	decision *runtimeIsolationDecision,
	observationErr error,
	now time.Time,
) error {
	runtimes := make([]string, 0, len(capabilities))
	profiles := make([]string, 0, len(capabilities))
	seenRuntimes := map[string]struct{}{}
	seenProfiles := map[string]struct{}{}
	expiresAt := now.Add(runtimeIsolationObservationTTL)
	for _, capability := range capabilities {
		if _, exists := seenRuntimes[capability.Runtime]; !exists {
			seenRuntimes[capability.Runtime] = struct{}{}
			runtimes = append(runtimes, capability.Runtime)
		}
		profile := string(capability.Profile)
		if _, exists := seenProfiles[profile]; !exists {
			seenProfiles[profile] = struct{}{}
			profiles = append(profiles, profile)
		}
		if capability.AttestationExpiresAt != nil && capability.AttestationExpiresAt.Before(expiresAt) {
			expiresAt = capability.AttestationExpiresAt.UTC()
		}
	}
	sort.Strings(runtimes)
	sort.Slice(profiles, func(left, right int) bool {
		return runtimeIsolationProfileRankString(profiles[left]) < runtimeIsolationProfileRankString(profiles[right])
	})

	state := "available"
	var reasonCode *string
	if observationErr != nil {
		state = "unattested"
		var problemErr *problem.Error
		if errors.As(observationErr, &problemErr) {
			reasonCode = runtimeIsolationStringPointer(problemErr.Code)
			if problemErr.Code == "gvisor_attestation_stale" {
				state = "stale"
			}
		} else {
			reasonCode = runtimeIsolationStringPointer("runtime_isolation_observation_failed")
		}
		if state != "stale" && decision != nil && decision.Decision == "fallback" {
			state = "degraded"
		}
	} else if decision != nil && decision.Decision == "fallback" {
		state = "degraded"
	}
	if !expiresAt.After(now) {
		expiresAt = now.Add(time.Second)
	}
	row := persistence.ExecutionTargetRuntimeIsolationObservation{
		ExecutionTargetID: targetID,
		DetectedRuntimes:  runtimes,
		DetectedProfiles:  profiles,
		State:             state,
		ReasonCode:        reasonCode,
		ObservedAt:        now.UTC(),
		ExpiresAt:         expiresAt.UTC(),
		UpdatedAt:         now.UTC(),
	}
	if decision == nil {
		return problem.New(500, "runtime_isolation_observation_invalid", "Runtime isolation availability omitted its selection decision.")
	}
	row.RequestedRuntime = decision.RequestedRuntime
	row.RequestedProfile = string(decision.RequestedProfile)
	row.PolicySource = decision.PolicySource
	row.Decision = decision.Decision
	if decision.EffectiveRuntime != "" {
		row.EffectiveRuntime = runtimeIsolationStringPointer(decision.EffectiveRuntime)
		row.EffectiveProfile = runtimeIsolationStringPointer(string(decision.EffectiveProfile))
	}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "execution_target_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"detected_runtimes", "detected_profiles", "requested_runtime", "requested_profile",
			"effective_runtime", "effective_profile", "policy_source", "decision", "state", "reason_code",
			"observed_at", "expires_at", "updated_at",
		}),
	}).Create(&row).Error; err != nil {
		return problem.Wrap(500, "runtime_isolation_observation_persist_failed", "Runtime isolation availability could not be recorded.", err)
	}
	return nil
}

func runtimeIsolationProfileRankString(profile string) int {
	return runtimeIsolationProfileRank(platform.ExecutionTargetIsolationProfile(profile))
}
