package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	CapacityAdmissionFixedUnboundedV1  = "fixed-unbounded-v1"
	CapacityAdmissionPublisherHealthV1 = "publisher-health-v1"
	CapacityAdmissionExactActiveV1     = "exact-active-v1"

	maximumReservationAcknowledgements = 4096
)

type reservationUnitCounts struct {
	Active         int64
	Unacknowledged int64
}

type reservationUnitCountsRow struct {
	ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id"`
	ActiveUnits       int64     `gorm:"column:active_units"`
	Unacknowledged    int64     `gorm:"column:unacknowledged_units"`
}

type normalizedReservationAuthority struct {
	Mode                   string
	Acknowledgements       []ReservationIdentity
	AcknowledgementsSHA256 string
}

// CapacityAdmission freezes the authority observed immediately before the
// new Execution row is inserted. The caller persists it after the Execution
// and Scheduling Decision have been created in the same transaction.
type CapacityAdmission struct {
	Evidence persistence.ExecutionCapacityAdmission
}

func prepareReservationAuthority(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	healthVersion int64,
	input *ReservationAuthorityObservation,
) (*normalizedReservationAuthority, error) {
	if input == nil {
		return nil, nil
	}
	mode := strings.TrimSpace(input.Mode)
	if mode != ReservationAuthorityExactActiveV1 {
		return nil, problem.New(
			400,
			"invalid_target_reservation_authority_mode",
			"Target reservation authority mode must be exact-active-v1.",
		)
	}
	if len(input.Acknowledgements) > maximumReservationAcknowledgements {
		return nil, problem.New(
			400,
			"target_reservation_acknowledgements_too_large",
			"A Target health observation supports at most 4096 reservation acknowledgements.",
		)
	}
	identities := append([]ReservationIdentity(nil), input.Acknowledgements...)
	sortReservationIdentities(identities)
	for index, identity := range identities {
		if identity.ExecutionID == uuid.Nil || identity.Generation < 0 {
			return nil, problem.New(
				400,
				"invalid_target_reservation_acknowledgement",
				"Reservation acknowledgements require an Execution ID and nonnegative generation.",
			)
		}
		if index > 0 && identities[index-1] == identity {
			return nil, problem.New(
				400,
				"duplicate_target_reservation_acknowledgement",
				"Reservation acknowledgements must be unique.",
			)
		}
	}

	type executionState struct {
		ID                uuid.UUID `gorm:"column:id"`
		ExecutionTargetID uuid.UUID `gorm:"column:execution_target_id"`
		Generation        int64     `gorm:"column:generation"`
		Status            string    `gorm:"column:status"`
	}
	ids := make([]uuid.UUID, 0, len(identities))
	for _, identity := range identities {
		ids = append(ids, identity.ExecutionID)
	}
	statesByID := make(map[uuid.UUID]executionState, len(ids))
	if len(ids) > 0 {
		var states []executionState
		if err := tx.WithContext(ctx).
			Model(&persistence.AgentExecution{}).
			Select("id", "execution_target_id", "generation", "status").
			Where("id IN ?", ids).
			Find(&states).Error; err != nil {
			return nil, problem.Wrap(
				500,
				"target_reservation_acknowledgements_load_failed",
				"Reservation acknowledgement Execution state could not be loaded.",
				err,
			)
		}
		for _, state := range states {
			statesByID[state.ID] = state
		}
	}
	active := make([]ReservationIdentity, 0, len(identities))
	for _, identity := range identities {
		state, found := statesByID[identity.ExecutionID]
		if !found || state.Generation != identity.Generation {
			// Claim, retention, and terminal transitions may race a publisher's
			// observation. A stale identity is harmless and is omitted from the
			// committed current-version acknowledgement set.
			continue
		}
		if state.ExecutionTargetID != targetID {
			return nil, problem.New(
				409,
				"target_reservation_acknowledgement_scope_mismatch",
				"A reservation acknowledgement belongs to another Execution Target.",
			)
		}
		if state.Status != "queued" && state.Status != "recovering" {
			continue
		}
		active = append(active, identity)
	}
	return &normalizedReservationAuthority{
		Mode:                   mode,
		Acknowledgements:       active,
		AcknowledgementsSHA256: ReservationAcknowledgementsSHA256(targetID, healthVersion, active),
	}, nil
}

func replaceReservationAcknowledgements(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
	healthVersion int64,
	acknowledgedAt time.Time,
	authority *normalizedReservationAuthority,
) error {
	if err := tx.WithContext(ctx).
		Where("execution_target_id = ?", targetID).
		Delete(&persistence.ExecutionTargetReservationAcknowledgement{}).Error; err != nil {
		return problem.Wrap(
			500,
			"target_reservation_acknowledgements_replace_failed",
			"Previous reservation acknowledgements could not be replaced.",
			err,
		)
	}
	if authority == nil || len(authority.Acknowledgements) == 0 {
		return nil
	}
	rows := make([]persistence.ExecutionTargetReservationAcknowledgement, 0, len(authority.Acknowledgements))
	for _, identity := range authority.Acknowledgements {
		rows = append(rows, persistence.ExecutionTargetReservationAcknowledgement{
			ExecutionTargetID: targetID, ExecutionID: identity.ExecutionID,
			ExecutionGeneration: identity.Generation, HealthVersion: healthVersion,
			AcknowledgedAt: acknowledgedAt,
		})
	}
	if err := tx.WithContext(ctx).Create(&rows).Error; err != nil {
		return problem.Wrap(
			409,
			"target_reservation_acknowledgements_replace_failed",
			"Current reservation acknowledgements could not be staged.",
			err,
		)
	}
	return nil
}

func sortReservationIdentities(identities []ReservationIdentity) {
	sort.Slice(identities, func(left, right int) bool {
		leftID := identities[left].ExecutionID.String()
		rightID := identities[right].ExecutionID.String()
		if leftID != rightID {
			return leftID < rightID
		}
		return identities[left].Generation < identities[right].Generation
	})
}

// ReservationAcknowledgementsSHA256 is the cross-dialect canonical digest of
// the exact acknowledgement set committed for one Health version.
func ReservationAcknowledgementsSHA256(
	targetID uuid.UUID,
	healthVersion int64,
	values []ReservationIdentity,
) string {
	identities := append([]ReservationIdentity(nil), values...)
	sortReservationIdentities(identities)
	var canonical strings.Builder
	canonical.WriteString("synara/capacity-reservation-acknowledgements/exact-active-v1\n")
	writeCapacityEvidenceField(&canonical, "execution_target_id", targetID.String())
	writeCapacityEvidenceField(&canonical, "health_version", strconv.FormatInt(healthVersion, 10))
	for _, identity := range identities {
		writeCapacityEvidenceField(&canonical, "execution_id", identity.ExecutionID.String())
		writeCapacityEvidenceField(&canonical, "execution_generation", strconv.FormatInt(identity.Generation, 10))
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(sum[:])
}

func loadReservationUnitCounts(
	ctx context.Context,
	db *gorm.DB,
	targetIDs []uuid.UUID,
) (map[uuid.UUID]reservationUnitCounts, error) {
	result := make(map[uuid.UUID]reservationUnitCounts, len(targetIDs))
	if len(targetIDs) == 0 {
		return result, nil
	}
	var rows []reservationUnitCountsRow
	err := db.WithContext(ctx).Table("agent_executions AS execution").
		Select(`execution.execution_target_id,
			COUNT(*) AS active_units,
			SUM(CASE WHEN acknowledgement.execution_id IS NULL THEN 1 ELSE 0 END) AS unacknowledged_units`).
		Joins(`LEFT JOIN execution_target_health AS health
			ON health.execution_target_id = execution.execution_target_id`).
		Joins(`LEFT JOIN execution_target_reservation_acknowledgements AS acknowledgement
			ON acknowledgement.execution_target_id = execution.execution_target_id
			AND acknowledgement.execution_id = execution.id
			AND acknowledgement.execution_generation = execution.generation
			AND acknowledgement.health_version = health.version`).
		Where("execution.execution_target_id IN ? AND execution.status IN ?", targetIDs, []string{"queued", "recovering"}).
		Group("execution.execution_target_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ExecutionTargetID == uuid.Nil || row.ActiveUnits < 0 || row.Unacknowledged < 0 {
			continue
		}
		result[row.ExecutionTargetID] = reservationUnitCounts{
			Active: row.ActiveUnits, Unacknowledged: row.Unacknowledged,
		}
	}
	return result, nil
}

func reservationAwareQueuePressure(
	health persistence.ExecutionTargetHealth,
	counts reservationUnitCounts,
	weight int,
) QueuePressureSnapshot {
	snapshot := QueuePressureSnapshot{
		QueuedExecutionUnits: counts.Active,
	}
	pressureUnits := counts.Active
	if health.ReservationAuthorityMode != nil &&
		*health.ReservationAuthorityMode == ReservationAuthorityExactActiveV1 {
		strictUsed := strictCapacityUsedUnits(health.AllocatedCapacityUnits, counts.Unacknowledged)
		snapshot.ReservationAuthorityMode = *health.ReservationAuthorityMode
		snapshot.ReservationAcknowledgedUnits = health.ReservationAcknowledgedUnits
		snapshot.UnacknowledgedReservationUnits = counts.Unacknowledged
		snapshot.StrictCapacityUsedUnits = &strictUsed
		pressureUnits = counts.Unacknowledged
	}
	snapshot.EffectiveLoadRank = effectiveLoadRankForPressure(health, pressureUnits, weight)
	return snapshot
}

// AdmitExecutionCapacity performs the final hard admission check under the
// already-held Target transaction lock. requireHealth is true for routed
// launches; fixed Targets preserve their legacy behavior until an exact
// reservation authority has explicitly been enabled.
func AdmitExecutionCapacity(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	executionID uuid.UUID,
	targetID uuid.UUID,
	requireHealth bool,
	admittedAt time.Time,
) (CapacityAdmission, error) {
	if tx == nil || tenantID == uuid.Nil || executionID == uuid.Nil || targetID == uuid.Nil || admittedAt.IsZero() {
		return CapacityAdmission{}, problem.New(
			500,
			"execution_capacity_admission_scope_invalid",
			"Capacity admission requires a transaction, Tenant, Execution, Target, and decision time.",
		)
	}
	var target persistence.ExecutionTarget
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select("id", "status").Where("id = ?", targetID).Take(&target).Error; err != nil {
		return CapacityAdmission{}, problem.Wrap(
			409,
			"execution_capacity_target_unavailable",
			"Execution Target capacity authority could not be locked.",
			err,
		)
	}
	if target.Status != "active" {
		return CapacityAdmission{}, problem.New(409, "execution_capacity_target_unavailable", "Execution Target is not active.")
	}

	countsByTarget, err := loadReservationUnitCounts(ctx, tx, []uuid.UUID{targetID})
	if err != nil {
		return CapacityAdmission{}, problem.Wrap(
			500,
			"execution_capacity_reservations_load_failed",
			"Active Target capacity reservations could not be counted.",
			err,
		)
	}
	counts := countsByTarget[targetID]
	evidence := persistence.ExecutionCapacityAdmission{
		TenantID: tenantID, ExecutionID: executionID, ExecutionTargetID: targetID,
		AdmissionMode:          CapacityAdmissionFixedUnboundedV1,
		ActiveReservationUnits: counts.Active, AdmittedAt: admittedAt.UTC().Truncate(time.Microsecond),
	}

	var health persistence.ExecutionTargetHealth
	healthErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("execution_target_id = ?", targetID).Take(&health).Error
	if healthErr != nil && !errors.Is(healthErr, gorm.ErrRecordNotFound) {
		return CapacityAdmission{}, problem.Wrap(
			500,
			"execution_capacity_health_load_failed",
			"Execution Target Health capacity authority could not be loaded.",
			healthErr,
		)
	}
	if requireHealth && errors.Is(healthErr, gorm.ErrRecordNotFound) {
		return CapacityAdmission{}, problem.New(409, "execution_capacity_health_unavailable", "Execution Target Health capacity authority is unavailable.")
	}
	if healthErr == nil {
		hasExactAuthority := health.ReservationAuthorityMode != nil &&
			*health.ReservationAuthorityMode == ReservationAuthorityExactActiveV1
		if requireHealth || hasExactAuthority {
			evidence.AdmissionMode = CapacityAdmissionPublisherHealthV1
			healthVersion := health.Version
			healthSource := health.Source
			healthObservedAt := health.ObservedAt
			healthExpiresAt := health.ExpiresAt
			capacityStatus := health.CapacityStatus
			allocated := health.AllocatedCapacityUnits
			evidence.HealthVersion = &healthVersion
			evidence.HealthSource = &healthSource
			evidence.HealthObservedAt = &healthObservedAt
			evidence.HealthExpiresAt = &healthExpiresAt
			evidence.CapacityStatus = &capacityStatus
			evidence.CapacityCeilingUnits = health.AvailableCapacityUnits
			evidence.AllocatedCapacityUnits = &allocated
		}
		if hasExactAuthority {
			if health.ObservedAt.After(admittedAt) || !health.ExpiresAt.After(admittedAt) ||
				(health.Status != HealthHealthy && health.Status != HealthDegraded) ||
				health.CapacityStatus == CapacitySaturated {
				return CapacityAdmission{}, problem.New(
					409,
					"execution_capacity_authority_unavailable",
					"The exact Target capacity reservation authority is not currently eligible.",
				)
			}
			strictUsed := strictCapacityUsedUnits(health.AllocatedCapacityUnits, counts.Unacknowledged)
			if health.AvailableCapacityUnits != nil && strictUsed >= int64(*health.AvailableCapacityUnits) {
				apiError := problem.New(
					409,
					"execution_target_capacity_reserved",
					"Execution Target capacity is fully consumed by allocated occupancy and unacknowledged reservations.",
				)
				apiError.Details = map[string]any{
					"executionTargetId":              targetID,
					"allocatedCapacityUnits":         health.AllocatedCapacityUnits,
					"unacknowledgedReservationUnits": counts.Unacknowledged,
					"capacityCeilingUnits":           *health.AvailableCapacityUnits,
				}
				return CapacityAdmission{}, apiError
			}
			mode := *health.ReservationAuthorityMode
			acknowledgedUnits := health.ReservationAcknowledgedUnits
			acknowledgementsSHA256 := ""
			if health.ReservationAcknowledgementsSHA256 != nil {
				acknowledgementsSHA256 = *health.ReservationAcknowledgementsSHA256
			}
			evidence.AdmissionMode = CapacityAdmissionExactActiveV1
			evidence.ReservationAuthorityMode = &mode
			evidence.ReservationAcknowledgedUnits = &acknowledgedUnits
			evidence.ReservationAcknowledgementsSHA256 = &acknowledgementsSHA256
			evidence.UnacknowledgedReservationUnits = &counts.Unacknowledged
			evidence.StrictCapacityUsedUnits = &strictUsed
		}
	}
	evidence.SnapshotSHA256 = CapacityAdmissionSHA256(evidence)
	return CapacityAdmission{Evidence: evidence}, nil
}

func strictCapacityUsedUnits(allocated int, unacknowledged int64) int64 {
	allocatedUnits := int64(allocated)
	if allocatedUnits < 0 || unacknowledged < 0 || unacknowledged > math.MaxInt64-allocatedUnits {
		return math.MaxInt64
	}
	return allocatedUnits + unacknowledged
}

func CreateCapacityAdmission(ctx context.Context, tx *gorm.DB, admission CapacityAdmission) error {
	if err := tx.WithContext(ctx).Create(&admission.Evidence).Error; err != nil {
		return problem.Wrap(
			409,
			"execution_capacity_admission_create_failed",
			"Immutable Execution capacity admission evidence could not be created.",
			err,
		)
	}
	var stored persistence.ExecutionCapacityAdmission
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ?", admission.Evidence.TenantID, admission.Evidence.ExecutionID).
		Take(&stored).Error; err != nil {
		return problem.Wrap(500, "execution_capacity_admission_verify_failed", "Capacity admission evidence could not be reloaded.", err)
	}
	if CapacityAdmissionSHA256(stored) != stored.SnapshotSHA256 {
		return problem.New(500, "execution_capacity_admission_digest_mismatch", "Capacity admission evidence failed digest verification.")
	}
	return nil
}

func CapacityAdmissionSHA256(value persistence.ExecutionCapacityAdmission) string {
	var canonical strings.Builder
	canonical.WriteString("synara/execution-capacity-admission/v1\n")
	writeCapacityEvidenceField(&canonical, "tenant_id", value.TenantID.String())
	writeCapacityEvidenceField(&canonical, "execution_id", value.ExecutionID.String())
	writeCapacityEvidenceField(&canonical, "execution_target_id", value.ExecutionTargetID.String())
	writeCapacityEvidenceField(&canonical, "admission_mode", value.AdmissionMode)
	writeCapacityEvidenceOptional(&canonical, "health_version", int64PointerString(value.HealthVersion))
	writeCapacityEvidenceOptional(&canonical, "health_source", value.HealthSource)
	writeCapacityEvidenceOptional(&canonical, "health_observed_at", timePointerString(value.HealthObservedAt))
	writeCapacityEvidenceOptional(&canonical, "health_expires_at", timePointerString(value.HealthExpiresAt))
	writeCapacityEvidenceOptional(&canonical, "capacity_status", value.CapacityStatus)
	writeCapacityEvidenceOptional(&canonical, "capacity_ceiling_units", intPointerString(value.CapacityCeilingUnits))
	writeCapacityEvidenceOptional(&canonical, "allocated_capacity_units", intPointerString(value.AllocatedCapacityUnits))
	writeCapacityEvidenceOptional(&canonical, "reservation_authority_mode", value.ReservationAuthorityMode)
	writeCapacityEvidenceOptional(&canonical, "reservation_acknowledged_units", intPointerString(value.ReservationAcknowledgedUnits))
	writeCapacityEvidenceOptional(&canonical, "reservation_acknowledgements_sha256", value.ReservationAcknowledgementsSHA256)
	writeCapacityEvidenceField(&canonical, "active_reservation_units", strconv.FormatInt(value.ActiveReservationUnits, 10))
	writeCapacityEvidenceOptional(&canonical, "unacknowledged_reservation_units", int64PointerString(value.UnacknowledgedReservationUnits))
	writeCapacityEvidenceOptional(&canonical, "strict_capacity_used_units", int64PointerString(value.StrictCapacityUsedUnits))
	writeCapacityEvidenceField(&canonical, "admitted_at", value.AdmittedAt.UTC().Format(time.RFC3339Nano))
	sum := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(sum[:])
}

func writeCapacityEvidenceField(builder *strings.Builder, name, value string) {
	builder.WriteString(name)
	builder.WriteByte(':')
	builder.WriteString(strconv.Itoa(len([]byte(value))))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte('\n')
}

func writeCapacityEvidenceOptional(builder *strings.Builder, name string, value *string) {
	if value == nil {
		builder.WriteString(name)
		builder.WriteString(":-\n")
		return
	}
	writeCapacityEvidenceField(builder, name, *value)
}

func intPointerString(value *int) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.Itoa(*value)
	return &formatted
}

func int64PointerString(value *int64) *string {
	if value == nil {
		return nil
	}
	formatted := strconv.FormatInt(*value, 10)
	return &formatted
}

func timePointerString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
