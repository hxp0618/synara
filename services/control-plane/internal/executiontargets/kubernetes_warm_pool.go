package executiontargets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type kubernetesWarmPool struct {
	ID                 uuid.UUID      `gorm:"column:id"`
	Version            int64          `gorm:"column:version"`
	CapacityClass      string         `gorm:"column:capacity_class"`
	ClusterID          string         `gorm:"column:cluster_id"`
	Namespace          string         `gorm:"column:namespace"`
	DesiredIdleUnits   int            `gorm:"column:desired_idle_units"`
	MaxActiveUnits     int            `gorm:"column:max_active_units"`
	SchedulingTemplate map[string]any `gorm:"column:scheduling_template"`
	Status             string         `gorm:"column:status"`
}

type kubernetesWarmWorkerState struct {
	WorkerID                uuid.UUID  `gorm:"column:id"`
	WorkerIncarnation       int64      `gorm:"column:incarnation"`
	PodName                 string     `gorm:"column:pod_name"`
	InstanceUID             string     `gorm:"column:instance_uid"`
	WorkerPoolID            *uuid.UUID `gorm:"column:worker_pool_id"`
	WorkerPoolVersion       *int64     `gorm:"column:worker_pool_version"`
	CapacityClass           *string    `gorm:"column:capacity_class"`
	WorkerReleaseRevisionID *uuid.UUID `gorm:"column:worker_release_revision_id"`
	WorkerReleaseChannel    *string    `gorm:"column:worker_release_channel"`
	WorkerReleaseStatus     string     `gorm:"column:worker_release_status"`
	RegistrationTrustMode   string     `gorm:"column:registration_trust_mode"`
	ProtocolVersion         int        `gorm:"column:protocol_version"`
	CurrentManifestID       *uuid.UUID `gorm:"column:current_manifest_id"`
	CompatibilityStatus     string     `gorm:"column:compatibility_status"`
	LeaseSupported          bool       `gorm:"column:lease_supported"`
	FencingSupported        bool       `gorm:"column:fencing_supported"`
	Status                  string     `gorm:"column:status"`
	AdministrativeStatus    string     `gorm:"column:administrative_status"`
	LastHeartbeatAt         time.Time  `gorm:"column:last_heartbeat_at"`
	HasLease                bool       `gorm:"column:has_lease"`
}

type kubernetesWarmWorkerIdentity struct {
	PodName     string
	InstanceUID string
}

type kubernetesWarmReleaseSelection struct {
	RevisionID  *uuid.UUID
	Channel     *string
	ImageDigest *string
}

type kubernetesWarmPodPlan struct {
	Pool       kubernetesWarmPool
	Slot       int
	Release    kubernetesWarmReleaseSelection
	ConfigHash string
}

type kubernetesObservedWarmPod struct {
	Pod           kubernetesPod
	PoolID        uuid.UUID
	PoolVersion   int64
	CapacityClass string
	Slot          int
	State         *kubernetesWarmWorkerState
}

type kubernetesWarmDemandEvictionCandidate struct {
	Observed kubernetesObservedWarmPod
	Key      kubernetesWarmCapacityKey
}

func managedKubernetesWarmCapacityObservations(
	tenantID uuid.UUID,
	executionTargetID uuid.UUID,
	warmPools []kubernetesWarmPool,
	warmPoolsSupported bool,
	warmClaimedCounts map[uuid.UUID]int,
	readyWarmCapacity map[kubernetesWarmCapacityKey]int,
	warmRelease kubernetesWarmReleaseSelection,
) []ManagedKubernetesWarmCapacityObservation {
	observations := make([]ManagedKubernetesWarmCapacityObservation, 0, len(warmPools))
	for _, pool := range warmPools {
		if pool.Status != placement.PoolStatusActive {
			continue
		}
		claimed := warmClaimedCounts[pool.ID]
		if claimed < 0 {
			claimed = 0
		}
		observation := ManagedKubernetesWarmCapacityObservation{
			TenantID:          tenantID,
			ExecutionTargetID: executionTargetID,
			WorkerPoolID:      pool.ID,
			WorkerPoolVersion: pool.Version,
			WarmSupported:     warmPoolsSupported,
			ClaimedUnits:      claimed,
		}
		if !warmPoolsSupported {
			observation.Reason = managedKubernetesRoutingReasonPointer(
				"Managed Kubernetes warm capacity is unavailable while a canary Worker release is active.",
			)
			observations = append(observations, observation)
			continue
		}
		observation.WorkerReleaseRevisionID = warmRelease.RevisionID
		observation.WorkerReleaseChannel = warmRelease.Channel
		observation.DesiredTotalUnits = pool.DesiredIdleUnits + claimed
		if observation.DesiredTotalUnits > pool.MaxActiveUnits {
			observation.DesiredTotalUnits = pool.MaxActiveUnits
		}
		key := kubernetesWarmCapacityKey{
			PoolID:          pool.ID,
			PoolVersion:     pool.Version,
			CapacityClass:   pool.CapacityClass,
			ReleaseRevision: optionalUUIDString(warmRelease.RevisionID),
			ReleaseChannel:  stringValue(warmRelease.Channel),
		}
		observation.ReadyIdleUnits = readyWarmCapacity[key]
		observations = append(observations, observation)
	}
	return observations
}

func kubernetesWarmWorkerStateForPod(
	states map[kubernetesWarmWorkerIdentity]kubernetesWarmWorkerState,
	pod kubernetesPod,
) (kubernetesWarmWorkerState, bool) {
	state, found := states[kubernetesWarmWorkerIdentityKey(pod.Name, pod.UID)]
	return state, found
}

func kubernetesWarmWorkerIdentityKey(podName, instanceUID string) kubernetesWarmWorkerIdentity {
	return kubernetesWarmWorkerIdentity{
		PodName:     strings.TrimSpace(podName),
		InstanceUID: strings.TrimSpace(instanceUID),
	}
}

func kubernetesWarmPodIdentity(pod kubernetesPod) (uuid.UUID, int64, string, int, error) {
	poolID, err := uuid.Parse(strings.TrimSpace(pod.Labels[kubernetesWorkerPoolIDLabel]))
	if err != nil {
		return uuid.UUID{}, 0, "", 0, err
	}
	poolVersion, err := strconv.ParseInt(strings.TrimSpace(pod.Labels[kubernetesWorkerPoolVersionLabel]), 10, 64)
	if err != nil || poolVersion <= 0 {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid worker pool version")
	}
	capacityClass := strings.TrimSpace(pod.Labels[kubernetesCapacityClassLabel])
	if capacityClass != placement.CapacityClassStandard && capacityClass != placement.CapacityClassInteractive {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid capacity class")
	}
	slot, err := strconv.Atoi(strings.TrimSpace(pod.Labels[kubernetesWarmSlotLabel]))
	if err != nil || slot < 0 {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid warm slot")
	}
	return poolID, poolVersion, capacityClass, slot, nil
}

func kubernetesWarmWorkerStateMatchesPlan(state kubernetesWarmWorkerState, plan kubernetesWarmPodPlan) bool {
	return state.WorkerPoolID != nil &&
		state.WorkerPoolVersion != nil &&
		state.CapacityClass != nil &&
		*state.WorkerPoolID == plan.Pool.ID &&
		*state.WorkerPoolVersion == plan.Pool.Version &&
		*state.CapacityClass == plan.Pool.CapacityClass &&
		sameOptionalUUID(state.WorkerReleaseRevisionID, plan.Release.RevisionID) &&
		sameOptionalString(state.WorkerReleaseChannel, plan.Release.Channel)
}

type kubernetesWarmCapacityKey struct {
	PoolID          uuid.UUID
	PoolVersion     int64
	CapacityClass   string
	ReleaseRevision string
	ReleaseChannel  string
}

func kubernetesWarmWorkerStateReadyIdle(
	state kubernetesWarmWorkerState,
	pod kubernetesPod,
	plan kubernetesWarmPodPlan,
	now time.Time,
	heartbeatTimeout time.Duration,
) bool {
	return kubernetesWarmWorkerIdentityKey(state.PodName, state.InstanceUID) ==
		kubernetesWarmWorkerIdentityKey(pod.Name, pod.UID) &&
		pod.Phase == "Running" &&
		!state.HasLease &&
		strings.TrimSpace(state.Status) == "online" &&
		strings.TrimSpace(state.AdministrativeStatus) == "active" &&
		state.ProtocolVersion == kubernetesWorkerProtocolVersion &&
		state.CurrentManifestID != nil &&
		strings.TrimSpace(state.CompatibilityStatus) == "compatible" &&
		state.LeaseSupported &&
		state.FencingSupported &&
		strings.TrimSpace(state.RegistrationTrustMode) == kubernetesPodBoundRegistrationTrust &&
		kubernetesWarmWorkerHeartbeatFresh(state.LastHeartbeatAt, now, heartbeatTimeout) &&
		kubernetesWarmWorkerStateMatchesPlan(state, plan) &&
		kubernetesWarmWorkerReleaseReady(state, plan)
}

func kubernetesWarmWorkerStateShouldRecycle(
	state kubernetesWarmWorkerState,
	pod kubernetesPod,
	now time.Time,
	heartbeatTimeout time.Duration,
) bool {
	if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
		return true
	}
	status := strings.TrimSpace(state.Status)
	administrativeStatus := strings.TrimSpace(state.AdministrativeStatus)
	return status == "offline" ||
		status == "draining" ||
		status == "terminated" ||
		administrativeStatus == "draining" ||
		!kubernetesWarmWorkerHeartbeatFresh(state.LastHeartbeatAt, now, heartbeatTimeout)
}

func kubernetesWarmWorkerHeartbeatFresh(lastHeartbeatAt, now time.Time, heartbeatTimeout time.Duration) bool {
	if heartbeatTimeout <= 0 || lastHeartbeatAt.IsZero() {
		return false
	}
	return !lastHeartbeatAt.Before(now.Add(-heartbeatTimeout))
}

func kubernetesWarmWorkerReleaseReady(state kubernetesWarmWorkerState, plan kubernetesWarmPodPlan) bool {
	planHasRevision := plan.Release.RevisionID != nil
	planHasChannel := plan.Release.Channel != nil
	if planHasRevision != planHasChannel ||
		!sameOptionalUUID(state.WorkerReleaseRevisionID, plan.Release.RevisionID) ||
		!sameOptionalString(state.WorkerReleaseChannel, plan.Release.Channel) {
		return false
	}
	status := strings.TrimSpace(state.WorkerReleaseStatus)
	if !planHasRevision {
		return status == "" || status == "unmanaged"
	}
	return status == "active"
}

func kubernetesWarmCapacityKeyForState(state kubernetesWarmWorkerState) (kubernetesWarmCapacityKey, bool) {
	if state.WorkerPoolID == nil || state.WorkerPoolVersion == nil || state.CapacityClass == nil {
		return kubernetesWarmCapacityKey{}, false
	}
	if (state.WorkerReleaseRevisionID == nil) != (state.WorkerReleaseChannel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: *state.WorkerPoolID, PoolVersion: *state.WorkerPoolVersion, CapacityClass: *state.CapacityClass,
		ReleaseRevision: optionalUUIDString(state.WorkerReleaseRevisionID),
		ReleaseChannel:  stringValue(state.WorkerReleaseChannel),
	}, true
}

func kubernetesWarmCapacityKeyForExecution(execution kubernetesExecution) (kubernetesWarmCapacityKey, bool) {
	if execution.WorkerPool == nil || execution.WorkerPool.Mode != placement.PoolModeWarm {
		return kubernetesWarmCapacityKey{}, false
	}
	if (execution.WorkerReleaseRevisionID == nil) != (execution.WorkerReleaseChannel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: execution.WorkerPool.ID, PoolVersion: execution.WorkerPool.Version, CapacityClass: execution.WorkerPool.CapacityClass,
		ReleaseRevision: optionalUUIDString(execution.WorkerReleaseRevisionID),
		ReleaseChannel:  stringValue(execution.WorkerReleaseChannel),
	}, true
}

func kubernetesWarmCapacityKeyForPlan(plan kubernetesWarmPodPlan) (kubernetesWarmCapacityKey, bool) {
	if (plan.Release.RevisionID == nil) != (plan.Release.Channel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: plan.Pool.ID, PoolVersion: plan.Pool.Version, CapacityClass: plan.Pool.CapacityClass,
		ReleaseRevision: optionalUUIDString(plan.Release.RevisionID),
		ReleaseChannel:  stringValue(plan.Release.Channel),
	}, true
}

func optionalUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func kubernetesWarmPodPlans(
	warmPools []kubernetesWarmPool,
	warmPoolsSupported bool,
	warmClaimedCounts map[uuid.UUID]int,
	warmRelease kubernetesWarmReleaseSelection,
	podBaseHash string,
	baseImage string,
) ([]kubernetesWarmPodPlan, map[string]kubernetesWarmPodPlan, error) {
	plans := make([]kubernetesWarmPodPlan, 0)
	plansByName := make(map[string]kubernetesWarmPodPlan)
	if !warmPoolsSupported {
		return plans, plansByName, nil
	}
	for _, pool := range warmPools {
		if pool.Status != placement.PoolStatusActive {
			continue
		}
		claimed := warmClaimedCounts[pool.ID]
		if claimed < 0 {
			claimed = 0
		}
		desiredTotal := pool.DesiredIdleUnits + claimed
		if desiredTotal > pool.MaxActiveUnits {
			desiredTotal = pool.MaxActiveUnits
		}
		for slot := 0; slot < desiredTotal; slot++ {
			plan := kubernetesWarmPodPlan{Pool: pool, Slot: slot, Release: warmRelease}
			configHash, err := kubernetesWarmPoolPodHash(podBaseHash, baseImage, plan)
			if err != nil {
				return nil, nil, err
			}
			plan.ConfigHash = configHash
			plans = append(plans, plan)
			plansByName[kubernetesWarmPodName(plan)] = plan
		}
	}
	return plans, plansByName, nil
}

func kubernetesWarmPodName(plan kubernetesWarmPodPlan) string {
	compactPoolID := strings.ReplaceAll(plan.Pool.ID.String(), "-", "")
	releaseKey := "unmanaged"
	if plan.Release.RevisionID != nil && plan.Release.Channel != nil {
		compactReleaseID := strings.ReplaceAll(plan.Release.RevisionID.String(), "-", "")
		channelKey := "p"
		if *plan.Release.Channel == "canary" {
			channelKey = "c"
		}
		releaseKey = channelKey + compactReleaseID[:8]
	}
	return fmt.Sprintf("synara-warm-%s-v%s-%s-s%d", compactPoolID[:10], strconv.FormatInt(plan.Pool.Version, 16), releaseKey, plan.Slot)
}

func kubernetesWarmPoolImage(baseImage string, release kubernetesWarmReleaseSelection) (string, error) {
	if release.RevisionID == nil && release.Channel == nil && release.ImageDigest == nil {
		return baseImage, nil
	}
	if release.RevisionID == nil || release.Channel == nil || release.ImageDigest == nil ||
		(*release.Channel != "promoted" && *release.Channel != "canary") {
		return "", problem.New(409, "worker_release_execution_invalid", "Warm Worker release selection is invalid.")
	}
	return pinImageReference(baseImage, *release.ImageDigest)
}

func kubernetesWarmPoolPodHash(baseHash, baseImage string, plan kubernetesWarmPodPlan) (string, error) {
	image, err := kubernetesWarmPoolImage(baseImage, plan.Release)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		BaseHash      string
		Image         string
		PoolID        uuid.UUID
		PoolVersion   int64
		CapacityClass string
		Slot          int
		RevisionID    *uuid.UUID
		Channel       *string
	}{baseHash, image, plan.Pool.ID, plan.Pool.Version, plan.Pool.CapacityClass, plan.Slot, plan.Release.RevisionID, plan.Release.Channel})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
