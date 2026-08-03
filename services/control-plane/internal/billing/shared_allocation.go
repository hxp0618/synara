package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	SharedAllocationAlgorithmClosedClaimIntervalV1 = "closed-claim-interval-v1"
	SharedAllocationKindTenantClaim                = "tenant-claim"
	SharedAllocationKindPlatformIdle               = "platform-idle"
)

type AllocateSharedUsageChargesInput struct {
	Provider             string
	CurrencyCode         string
	WorkerID             uuid.UUID
	WorkerIncarnation    int64
	BillingPeriodStartAt time.Time
	BillingPeriodEndAt   time.Time
}

type SharedUsageAllocationResult struct {
	Run    *persistence.BillingSharedCostAllocationRun
	Slices []persistence.BillingSharedEstimatedChargeSlice
}

type sharedClaimInterval struct {
	Claim   persistence.WorkerClaimFact
	Release persistence.WorkerClaimReleaseFact
	StartAt time.Time
	EndAt   time.Time
}

type sharedOwnershipInterval struct {
	AllocationKind string
	TenantID       *uuid.UUID
	ClaimFactID    *uuid.UUID
	StartAt        time.Time
	EndAt          time.Time
}

type sharedTimeChargeCandidate struct {
	Kind        string
	RateMicros  int64
	Resource    *int64
	Denominator int64
}

// AllocateSharedUsageCharges freezes a deterministic cost allocation for one
// terminal, platform-shared Worker incarnation. It intentionally remains
// separate from EstimateUsageCharges: tenant-owned Workers use one immutable
// tenant attribution, while shared Workers require complete closed claim
// intervals and preserve platform-idle cost explicitly.
func (s *Service) AllocateSharedUsageCharges(
	ctx context.Context,
	input AllocateSharedUsageChargesInput,
) (SharedUsageAllocationResult, error) {
	provider, err := normalizeProvider(input.Provider)
	if err != nil {
		return SharedUsageAllocationResult{}, err
	}
	currency, err := normalizeCurrency(input.CurrencyCode)
	if err != nil {
		return SharedUsageAllocationResult{}, err
	}
	periodStart, periodEnd, err := normalizeClosedPeriod(input.BillingPeriodStartAt, input.BillingPeriodEndAt)
	if err != nil {
		return SharedUsageAllocationResult{}, err
	}
	if input.WorkerID == uuid.Nil || input.WorkerIncarnation <= 0 {
		return SharedUsageAllocationResult{}, problem.New(
			400,
			"invalid_cost_accounting_worker",
			"workerId and workerIncarnation are required.",
		)
	}
	// The SQLite deployment profile is deliberately single-replica, but HTTP
	// handlers can still call the same Service concurrently. Serialize the
	// check/create/reload sequence so a concurrent identical request replays the
	// winner instead of leaking SQLITE_BUSY or a unique-key error.
	if s.db.Dialector.Name() != "postgres" {
		s.sharedAllocationMu.Lock()
		defer s.sharedAllocationMu.Unlock()
	}

	result := SharedUsageAllocationResult{}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if lockErr := acquireSharedAllocationIdentityLock(
			ctx,
			tx,
			input.WorkerID,
			input.WorkerIncarnation,
			provider,
			currency,
		); lockErr != nil {
			return problem.Wrap(
				500,
				"cost_accounting_shared_allocation_lock_failed",
				"The shared billing allocation identity could not be locked.",
				lockErr,
			)
		}
		if lockErr := acquireSharedEstimatePeriodSnapshotLock(
			ctx,
			tx,
			provider,
			currency,
			periodStart,
			periodEnd,
		); lockErr != nil {
			return problem.Wrap(
				500,
				"cost_accounting_shared_allocation_snapshot_lock_failed",
				"The shared billing period snapshot could not be locked.",
				lockErr,
			)
		}

		fact, loadErr := loadWorkerFactForBilling(ctx, tx, input.WorkerID, input.WorkerIncarnation)
		if loadErr != nil {
			return loadErr
		}
		if fact.TenantID != nil {
			return problem.New(
				409,
				"cost_accounting_shared_worker_tenant_attributed",
				"A tenant-attributed Worker must use the ordinary usage-estimate path.",
			)
		}
		if fact.CurrentState != "terminated" || fact.TerminatedAt == nil || fact.TerminalReason == nil {
			return problem.New(
				409,
				"cost_accounting_shared_worker_not_terminal",
				"Shared cost allocation requires an immutable terminal Worker incarnation fact.",
			)
		}

		usageStart := maxTime(periodStart, fact.RegisteredAt.UTC())
		usageEnd := periodEnd
		if fact.TerminatedAt.UTC().Before(usageEnd) {
			usageEnd = fact.TerminatedAt.UTC()
		}
		if !usageEnd.After(usageStart) {
			return nil
		}

		now := s.now().UTC()
		if now.Before(usageEnd) {
			return problem.New(
				409,
				"cost_accounting_shared_worker_terminal_in_future",
				"The terminal Worker usage window ends after the allocation authority timestamp.",
			)
		}

		target, targetErr := loadSharedAllocationTarget(ctx, tx, fact.ExecutionTargetID)
		if targetErr != nil {
			return targetErr
		}
		if target.TenantID != nil || target.Kind != fact.TargetKind {
			return problem.New(
				409,
				"cost_accounting_shared_target_scope_invalid",
				"The Worker incarnation does not belong to a platform-shared execution Target.",
			)
		}

		coverage, coverageErr := loadSharedLedgerCoverage(ctx, tx, fact.ExecutionTargetID)
		if coverageErr != nil {
			return coverageErr
		}
		if fact.RegisteredAt.UTC().Before(coverage.CompleteFromAt.UTC()) || coverage.SealedAt.UTC().After(now) {
			return problem.New(
				409,
				"cost_accounting_shared_ledger_coverage_incomplete",
				"The shared Target ledger coverage does not authorize this Worker incarnation.",
			)
		}
		if overlapErr := rejectOverlappingSharedAllocationPeriod(
			ctx,
			tx,
			fact.WorkerID,
			fact.WorkerIncarnation,
			provider,
			currency,
			periodStart,
			periodEnd,
		); overlapErr != nil {
			return overlapErr
		}

		claims, releases, intervals, ledgerSHA256, ledgerErr := loadAndValidateSharedClaimLedger(ctx, tx, fact)
		if ledgerErr != nil {
			return ledgerErr
		}

		tariffs, tariffErr := loadCandidateTariffs(
			ctx,
			tx,
			provider,
			fact.Region,
			currency,
			usageStart,
			usageEnd,
		)
		if tariffErr != nil {
			return tariffErr
		}
		segments, segmentErr := buildChargeSegments(fact.Region, usageStart, usageEnd, tariffs)
		if segmentErr != nil {
			return segmentErr
		}
		segments = mergeAdjacentSharedChargeSegments(segments)

		run, slices, buildErr := buildSharedAllocationRows(
			fact,
			coverage,
			provider,
			currency,
			periodStart,
			periodEnd,
			usageStart,
			usageEnd,
			claims,
			releases,
			intervals,
			ledgerSHA256,
			segments,
			now,
		)
		if buildErr != nil {
			return buildErr
		}

		existingRun, found, existingErr := loadSharedAllocationRun(
			ctx,
			tx,
			fact.WorkerID,
			fact.WorkerIncarnation,
			provider,
			currency,
			periodStart,
			periodEnd,
		)
		if existingErr != nil {
			return existingErr
		}
		if found {
			existingSlices, sliceErr := loadSharedAllocationSlices(ctx, tx, existingRun.ID)
			if sliceErr != nil {
				return sliceErr
			}
			if !sameSharedAllocationRun(existingRun, run) || !sameSharedAllocationSlices(existingSlices, slices) {
				return problem.New(
					409,
					"cost_accounting_shared_allocation_conflict",
					"The existing shared billing allocation does not match the authoritative ledger and tariff evidence.",
				)
			}
			runCopy := existingRun
			result = SharedUsageAllocationResult{Run: &runCopy, Slices: existingSlices}
			return nil
		}

		if createErr := tx.WithContext(ctx).Omit(clause.Associations).Create(&run).Error; createErr != nil {
			return problem.Wrap(
				409,
				"cost_accounting_shared_allocation_create_failed",
				"The shared billing allocation run could not be created.",
				createErr,
			)
		}
		if len(slices) > 0 {
			if createErr := tx.WithContext(ctx).Omit(clause.Associations).Create(&slices).Error; createErr != nil {
				return problem.Wrap(
					409,
					"cost_accounting_shared_allocation_slice_create_failed",
					"The shared billing allocation slices could not be created.",
					createErr,
				)
			}
		}
		runCopy := run
		result = SharedUsageAllocationResult{Run: &runCopy, Slices: slices}
		return nil
	})
	if err != nil {
		return SharedUsageAllocationResult{}, err
	}
	return result, nil
}

func acquireSharedAllocationIdentityLock(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
	provider string,
	currency string,
) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	lockKey := strings.Join([]string{
		"synara:billing-shared-cost-allocation",
		workerID.String(),
		fmt.Sprintf("%d", workerIncarnation),
		provider,
		currency,
		SharedAllocationAlgorithmClosedClaimIntervalV1,
	}, "\x1f")
	return tx.WithContext(ctx).
		Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", lockKey).
		Error
}

func rejectOverlappingSharedAllocationPeriod(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
	provider string,
	currency string,
	periodStart time.Time,
	periodEnd time.Time,
) error {
	var count int64
	err := tx.WithContext(ctx).
		Model(&persistence.BillingSharedCostAllocationRun{}).
		Where(
			"worker_id = ? AND worker_incarnation = ? AND provider = ? AND currency_code = ? AND algorithm_version = ?",
			workerID,
			workerIncarnation,
			provider,
			currency,
			SharedAllocationAlgorithmClosedClaimIntervalV1,
		).
		Where("billing_period_start_at < ? AND billing_period_end_at > ?", periodEnd, periodStart).
		Where("NOT (billing_period_start_at = ? AND billing_period_end_at = ?)", periodStart, periodEnd).
		Count(&count).Error
	if err != nil {
		return problem.Wrap(
			500,
			"cost_accounting_shared_allocation_overlap_probe_failed",
			"The shared billing allocation period overlap check could not be completed.",
			err,
		)
	}
	if count > 0 {
		return problem.New(
			409,
			"cost_accounting_shared_allocation_period_overlap",
			"The requested shared billing allocation period overlaps an immutable existing run.",
		)
	}
	return nil
}

func loadSharedAllocationTarget(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
) (persistence.ExecutionTarget, error) {
	var target persistence.ExecutionTarget
	err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("id = ?", targetID).
		Take(&target).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(
			404,
			"cost_accounting_shared_target_not_found",
			"The shared billing execution Target was not found.",
		)
	}
	if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(
			500,
			"cost_accounting_shared_target_load_failed",
			"The shared billing execution Target could not be loaded.",
			err,
		)
	}
	return target, nil
}

func loadSharedLedgerCoverage(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
) (persistence.BillingSharedTargetLedgerCoverage, error) {
	var coverage persistence.BillingSharedTargetLedgerCoverage
	err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("execution_target_id = ?", targetID).
		Take(&coverage).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.BillingSharedTargetLedgerCoverage{}, problem.New(
			409,
			"cost_accounting_shared_ledger_coverage_missing",
			"The shared Target does not have an operator-sealed complete claim/release-ledger boundary.",
		)
	}
	if err != nil {
		return persistence.BillingSharedTargetLedgerCoverage{}, problem.Wrap(
			500,
			"cost_accounting_shared_ledger_coverage_load_failed",
			"The shared Target ledger coverage could not be loaded.",
			err,
		)
	}
	return coverage, nil
}

func loadAndValidateSharedClaimLedger(
	ctx context.Context,
	tx *gorm.DB,
	fact persistence.WorkerIncarnationFact,
) (
	[]persistence.WorkerClaimFact,
	[]persistence.WorkerClaimReleaseFact,
	[]sharedClaimInterval,
	string,
	error,
) {
	claims := make([]persistence.WorkerClaimFact, 0)
	if err := tx.WithContext(ctx).
		Where("worker_id = ? AND worker_incarnation = ?", fact.WorkerID, fact.WorkerIncarnation).
		Order("claimed_at ASC, id ASC").
		Find(&claims).Error; err != nil {
		return nil, nil, nil, "", problem.Wrap(
			500,
			"cost_accounting_shared_claim_ledger_load_failed",
			"The shared Worker claim ledger could not be loaded.",
			err,
		)
	}
	if int64(len(claims)) != fact.ClaimCount {
		return nil, nil, nil, "", problem.New(
			409,
			"cost_accounting_shared_claim_ledger_incomplete",
			"The shared Worker claim count does not match its immutable claim ledger.",
		)
	}

	releases := make([]persistence.WorkerClaimReleaseFact, 0, len(claims))
	if len(claims) > 0 {
		claimIDs := make([]uuid.UUID, 0, len(claims))
		for _, claim := range claims {
			claimIDs = append(claimIDs, claim.ID)
		}
		if err := tx.WithContext(ctx).
			Where("claim_fact_id IN ?", claimIDs).
			Order("claim_fact_id ASC").
			Find(&releases).Error; err != nil {
			return nil, nil, nil, "", problem.Wrap(
				500,
				"cost_accounting_shared_release_ledger_load_failed",
				"The shared Worker release ledger could not be loaded.",
				err,
			)
		}
	}
	if len(releases) != len(claims) {
		return nil, nil, nil, "", problem.New(
			409,
			"cost_accounting_shared_release_ledger_incomplete",
			"Every shared Worker claim requires one immutable release fact before allocation.",
		)
	}

	releaseByClaim := make(map[uuid.UUID]persistence.WorkerClaimReleaseFact, len(releases))
	for _, release := range releases {
		releaseByClaim[release.ClaimFactID] = release
	}
	terminatedAt := fact.TerminatedAt.UTC()
	intervals := make([]sharedClaimInterval, 0, len(claims))
	for _, claim := range claims {
		release, found := releaseByClaim[claim.ID]
		if !found {
			return nil, nil, nil, "", problem.New(
				409,
				"cost_accounting_shared_release_ledger_incomplete",
				"Every shared Worker claim requires one immutable release fact before allocation.",
			)
		}
		claimedAt := claim.ClaimedAt.UTC()
		releasedAt := release.ReleasedAt.UTC()
		if claim.ExecutionTargetID != fact.ExecutionTargetID ||
			claim.TargetKind != fact.TargetKind ||
			claimedAt.Before(fact.RegisteredAt.UTC()) ||
			claimedAt.After(terminatedAt) ||
			releasedAt.Before(claimedAt) ||
			release.RecordedAt.UTC().Before(releasedAt) {
			return nil, nil, nil, "", problem.New(
				409,
				"cost_accounting_shared_claim_interval_invalid",
				"The shared Worker claim/release ledger contains an invalid scope or timeline.",
			)
		}
		intervalEnd := releasedAt
		if terminatedAt.Before(intervalEnd) {
			intervalEnd = terminatedAt
		}
		intervals = append(intervals, sharedClaimInterval{
			Claim: claim, Release: release, StartAt: claimedAt, EndAt: intervalEnd,
		})
	}

	sort.Slice(intervals, func(i, j int) bool {
		if !intervals[i].StartAt.Equal(intervals[j].StartAt) {
			return intervals[i].StartAt.Before(intervals[j].StartAt)
		}
		return intervals[i].Claim.ID.String() < intervals[j].Claim.ID.String()
	})
	for index := 1; index < len(intervals); index++ {
		if intervals[index].StartAt.Before(intervals[index-1].EndAt) {
			return nil, nil, nil, "", problem.New(
				409,
				"cost_accounting_shared_claim_intervals_overlap",
				"Shared Worker claims overlap and cannot be allocated to more than one tenant.",
			)
		}
	}

	ledgerSHA256, err := sharedClaimLedgerSHA256(fact, intervals)
	if err != nil {
		return nil, nil, nil, "", problem.Wrap(
			500,
			"cost_accounting_shared_ledger_digest_failed",
			"The shared Worker claim/release ledger digest could not be computed.",
			err,
		)
	}
	return claims, releases, intervals, ledgerSHA256, nil
}

type sharedLedgerDigestRow struct {
	ClaimID                   string `json:"claimId"`
	WorkerID                  string `json:"workerId"`
	WorkerIncarnation         int64  `json:"workerIncarnation"`
	TenantID                  string `json:"tenantId"`
	ExecutionTargetID         string `json:"executionTargetId"`
	TargetKind                string `json:"targetKind"`
	ClaimKind                 string `json:"claimKind"`
	RequestID                 string `json:"requestId"`
	ClaimedAt                 string `json:"claimedAt"`
	ClaimCreatedAt            string `json:"claimCreatedAt"`
	ExecutionID               string `json:"executionId"`
	ExecutionGeneration       string `json:"executionGeneration"`
	CleanupCommandID          string `json:"cleanupCommandId"`
	CleanupDispatchGeneration string `json:"cleanupDispatchGeneration"`
	ReleasedAt                string `json:"releasedAt"`
	RecordedAt                string `json:"recordedAt"`
	ReleaseReason             string `json:"releaseReason"`
	AuthorityKind             string `json:"authorityKind"`
	AuthorityID               string `json:"authorityId"`
	ReleaseRequestID          string `json:"releaseRequestId"`
	ReleaseMetadata           string `json:"releaseMetadata"`
}

func sharedClaimLedgerSHA256(
	fact persistence.WorkerIncarnationFact,
	intervals []sharedClaimInterval,
) (string, error) {
	rows := make([]sharedLedgerDigestRow, 0, len(intervals))
	for _, interval := range intervals {
		metadata, err := json.Marshal(interval.Release.Metadata)
		if err != nil {
			return "", fmt.Errorf("marshal claim %s release metadata: %w", interval.Claim.ID, err)
		}
		rows = append(rows, sharedLedgerDigestRow{
			ClaimID:                   interval.Claim.ID.String(),
			WorkerID:                  fact.WorkerID.String(),
			WorkerIncarnation:         fact.WorkerIncarnation,
			TenantID:                  interval.Claim.TenantID.String(),
			ExecutionTargetID:         interval.Claim.ExecutionTargetID.String(),
			TargetKind:                interval.Claim.TargetKind,
			ClaimKind:                 interval.Claim.ClaimKind,
			RequestID:                 interval.Claim.RequestID,
			ClaimedAt:                 interval.Claim.ClaimedAt.UTC().Format(time.RFC3339Nano),
			ClaimCreatedAt:            interval.Claim.CreatedAt.UTC().Format(time.RFC3339Nano),
			ExecutionID:               sharedOptionalUUIDString(interval.Claim.ExecutionID),
			ExecutionGeneration:       sharedOptionalInt64String(interval.Claim.ExecutionGeneration),
			CleanupCommandID:          sharedOptionalUUIDString(interval.Claim.CleanupCommandID),
			CleanupDispatchGeneration: sharedOptionalInt64String(interval.Claim.CleanupDispatchGeneration),
			ReleasedAt:                interval.Release.ReleasedAt.UTC().Format(time.RFC3339Nano),
			RecordedAt:                interval.Release.RecordedAt.UTC().Format(time.RFC3339Nano),
			ReleaseReason:             interval.Release.ReleaseReason,
			AuthorityKind:             interval.Release.AuthorityKind,
			AuthorityID:               sharedOptionalString(interval.Release.AuthorityID),
			ReleaseRequestID:          sharedOptionalString(interval.Release.RequestID),
			ReleaseMetadata:           string(metadata),
		})
	}
	payload, err := json.Marshal(struct {
		Algorithm string                  `json:"algorithm"`
		Rows      []sharedLedgerDigestRow `json:"rows"`
	}{Algorithm: SharedAllocationAlgorithmClosedClaimIntervalV1, Rows: rows})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func buildSharedAllocationRows(
	fact persistence.WorkerIncarnationFact,
	coverage persistence.BillingSharedTargetLedgerCoverage,
	provider string,
	currency string,
	periodStart time.Time,
	periodEnd time.Time,
	usageStart time.Time,
	usageEnd time.Time,
	claims []persistence.WorkerClaimFact,
	releases []persistence.WorkerClaimReleaseFact,
	intervals []sharedClaimInterval,
	ledgerSHA256 string,
	segments []chargeSegment,
	createdAt time.Time,
) (persistence.BillingSharedCostAllocationRun, []persistence.BillingSharedEstimatedChargeSlice, error) {
	run := persistence.BillingSharedCostAllocationRun{
		ExecutionTargetID:    fact.ExecutionTargetID,
		WorkerID:             fact.WorkerID,
		WorkerIncarnation:    fact.WorkerIncarnation,
		LedgerCoverageID:     coverage.ID,
		Provider:             provider,
		Region:               strings.TrimSpace(fact.Region),
		CurrencyCode:         currency,
		BillingPeriodStartAt: periodStart,
		BillingPeriodEndAt:   periodEnd,
		AlgorithmVersion:     SharedAllocationAlgorithmClosedClaimIntervalV1,
		UsageStartAt:         usageStart,
		UsageEndAt:           usageEnd,
		ClaimCount:           int64(len(claims)),
		ReleaseCount:         int64(len(releases)),
		LedgerSHA256:         ledgerSHA256,
		CreatedAt:            createdAt,
	}
	run.ID = deterministicSharedAllocationRunID(run)

	resourceKey := workerResourceCorrelationKey(fact)
	slices := make([]persistence.BillingSharedEstimatedChargeSlice, 0)
	for _, segment := range segments {
		ownership, err := buildSharedOwnershipIntervals(segment, intervals)
		if err != nil {
			return persistence.BillingSharedCostAllocationRun{}, nil, err
		}
		for _, owner := range ownership {
			startOffset := wholeSecondsBetween(segment.StartAt, owner.StartAt)
			endOffset := wholeSecondsBetween(segment.StartAt, owner.EndAt)
			billableSeconds := endOffset - startOffset
			if billableSeconds < 0 {
				return persistence.BillingSharedCostAllocationRun{}, nil, problem.New(
					500,
					"cost_accounting_shared_allocation_seconds_invalid",
					"A shared billing allocation interval produced a negative duration.",
				)
			}
			if owner.AllocationKind == SharedAllocationKindTenantClaim {
				run.TenantAllocatedSeconds, err = addInt64Checked(run.TenantAllocatedSeconds, billableSeconds)
			} else {
				run.PlatformIdleSeconds, err = addInt64Checked(run.PlatformIdleSeconds, billableSeconds)
			}
			if err != nil {
				return persistence.BillingSharedCostAllocationRun{}, nil, err
			}

			for _, candidate := range sharedTimeChargeCandidates(fact, segment.Tariff) {
				if candidate.RateMicros <= 0 {
					continue
				}
				amountStart, amountErr := sharedTimeChargeAmountAt(candidate, startOffset)
				if amountErr != nil {
					return persistence.BillingSharedCostAllocationRun{}, nil, amountErr
				}
				amountEnd, amountErr := sharedTimeChargeAmountAt(candidate, endOffset)
				if amountErr != nil {
					return persistence.BillingSharedCostAllocationRun{}, nil, amountErr
				}
				amountMicros := amountEnd - amountStart
				row := newSharedAllocationSlice(
					run.ID,
					fact,
					segment.Tariff,
					owner.AllocationKind,
					owner.TenantID,
					owner.ClaimFactID,
					candidate.Kind,
					resourceKey,
					periodStart,
					periodEnd,
					owner.StartAt,
					owner.EndAt,
					billableSeconds,
					sharedAllocationClaimCount(owner.AllocationKind),
					candidate.RateMicros,
					amountMicros,
					createdAt,
				)
				slices = append(slices, row)
			}
		}
	}
	allocatedSeconds, err := addInt64Checked(run.TenantAllocatedSeconds, run.PlatformIdleSeconds)
	if err != nil {
		return persistence.BillingSharedCostAllocationRun{}, nil, err
	}
	if allocatedSeconds != wholeSecondsBetween(usageStart, usageEnd) {
		return persistence.BillingSharedCostAllocationRun{}, nil, problem.New(
			409,
			"cost_accounting_shared_allocation_second_precision_ambiguous",
			"Tariff boundaries cannot conserve the complete shared Worker usage window at whole-second precision.",
		)
	}

	includeFinalEnd := fact.TerminatedAt != nil && fact.TerminatedAt.UTC().Equal(usageEnd)
	for _, claim := range claims {
		claimedAt := claim.ClaimedAt.UTC()
		if !sharedClaimInsideUsageWindow(claimedAt, usageStart, usageEnd, includeFinalEnd) {
			continue
		}
		segmentIndex, found := findChargeSegmentForClaim(claimedAt, segments, includeFinalEnd)
		if !found {
			return persistence.BillingSharedCostAllocationRun{}, nil, problem.New(
				500,
				"cost_accounting_shared_request_claim_segment_missing",
				"A shared Worker request claim did not match a cost tariff segment.",
			)
		}
		segment := segments[segmentIndex]
		if segment.Tariff.RequestRateMicros <= 0 {
			continue
		}
		tenantID := claim.TenantID
		claimID := claim.ID
		row := newSharedAllocationSlice(
			run.ID,
			fact,
			segment.Tariff,
			SharedAllocationKindTenantClaim,
			&tenantID,
			&claimID,
			ChargeKindRequest,
			resourceKey,
			periodStart,
			periodEnd,
			segment.StartAt,
			segment.EndAt,
			0,
			1,
			segment.Tariff.RequestRateMicros,
			segment.Tariff.RequestRateMicros,
			createdAt,
		)
		slices = append(slices, row)
	}

	sortSharedAllocationSlices(slices)
	return run, slices, nil
}

func buildSharedOwnershipIntervals(
	segment chargeSegment,
	claims []sharedClaimInterval,
) ([]sharedOwnershipInterval, error) {
	result := make([]sharedOwnershipInterval, 0, len(claims)*2+1)
	cursor := segment.StartAt
	for _, interval := range claims {
		if !interval.EndAt.After(segment.StartAt) || !segment.EndAt.After(interval.StartAt) {
			continue
		}
		startAt := maxTime(segment.StartAt, interval.StartAt)
		endAt := minTime(segment.EndAt, interval.EndAt)
		if !endAt.After(startAt) {
			continue
		}
		if startAt.Before(cursor) {
			return nil, problem.New(
				409,
				"cost_accounting_shared_claim_intervals_overlap",
				"Shared Worker claims overlap and cannot be allocated to more than one tenant.",
			)
		}
		if startAt.After(cursor) {
			result = append(result, sharedOwnershipInterval{
				AllocationKind: SharedAllocationKindPlatformIdle,
				StartAt:        cursor,
				EndAt:          startAt,
			})
		}
		tenantID := interval.Claim.TenantID
		claimID := interval.Claim.ID
		result = append(result, sharedOwnershipInterval{
			AllocationKind: SharedAllocationKindTenantClaim,
			TenantID:       &tenantID,
			ClaimFactID:    &claimID,
			StartAt:        startAt,
			EndAt:          endAt,
		})
		cursor = endAt
	}
	if cursor.Before(segment.EndAt) {
		result = append(result, sharedOwnershipInterval{
			AllocationKind: SharedAllocationKindPlatformIdle,
			StartAt:        cursor,
			EndAt:          segment.EndAt,
		})
	}
	return result, nil
}

// buildChargeSegments includes every candidate-tariff boundary, including a
// global fallback boundary hidden beneath one continuously effective regional
// tariff. Shared allocation merges those no-op boundaries so whole-second and
// monetary rounding reset only when the selected tariff actually changes.
func mergeAdjacentSharedChargeSegments(segments []chargeSegment) []chargeSegment {
	if len(segments) < 2 {
		return segments
	}
	merged := make([]chargeSegment, 0, len(segments))
	for _, segment := range segments {
		last := len(merged) - 1
		if last >= 0 && merged[last].Tariff.ID == segment.Tariff.ID && merged[last].EndAt.Equal(segment.StartAt) {
			merged[last].EndAt = segment.EndAt
			continue
		}
		merged = append(merged, segment)
	}
	return merged
}

func sharedTimeChargeCandidates(
	fact persistence.WorkerIncarnationFact,
	tariff persistence.BillingProviderTariff,
) []sharedTimeChargeCandidate {
	return []sharedTimeChargeCandidate{
		{
			Kind: ChargeKindCPU, RateMicros: tariff.CPUCoreHourRateMicros,
			Resource: fact.RequestedCPUMillicores, Denominator: 1000 * 3600,
		},
		{
			Kind: ChargeKindMemory, RateMicros: tariff.MemoryGiBHourRateMicros,
			Resource: fact.RequestedMemoryBytes, Denominator: (1 << 30) * 3600,
		},
		{
			Kind: ChargeKindEphemeralStorage, RateMicros: tariff.EphemeralGiBHourRateMicros,
			Resource: fact.RequestedEphemeralStorageBytes, Denominator: (1 << 30) * 3600,
		},
		{
			Kind: ChargeKindPod, RateMicros: tariff.PodHourRateMicros,
			Denominator: 3600,
		},
	}
}

func sharedTimeChargeAmountAt(candidate sharedTimeChargeCandidate, seconds int64) (int64, error) {
	parts := []int64{candidate.RateMicros, seconds}
	if candidate.Kind != ChargeKindPod {
		if candidate.Resource == nil || *candidate.Resource <= 0 {
			return 0, problem.New(
				409,
				"cost_accounting_shared_requested_resource_invalid",
				"A shared Worker requested resource value is missing or invalid.",
			)
		}
		parts = []int64{candidate.RateMicros, *candidate.Resource, seconds}
	}
	return multiplyDivideRound(parts, candidate.Denominator)
}

func newSharedAllocationSlice(
	runID uuid.UUID,
	fact persistence.WorkerIncarnationFact,
	tariff persistence.BillingProviderTariff,
	allocationKind string,
	tenantID *uuid.UUID,
	claimFactID *uuid.UUID,
	chargeKind string,
	resourceKey string,
	periodStart time.Time,
	periodEnd time.Time,
	usageStart time.Time,
	usageEnd time.Time,
	billableSeconds int64,
	claimCount int64,
	rateMicros int64,
	amountMicros int64,
	createdAt time.Time,
) persistence.BillingSharedEstimatedChargeSlice {
	row := persistence.BillingSharedEstimatedChargeSlice{
		RunID:                          runID,
		TenantID:                       cloneOptionalUUID(tenantID),
		ClaimFactID:                    cloneOptionalUUID(claimFactID),
		TariffID:                       tariff.ID,
		ChargeKind:                     chargeKind,
		AllocationKind:                 allocationKind,
		ResourceCorrelationKey:         resourceKey,
		BillingPeriodStartAt:           periodStart,
		BillingPeriodEndAt:             periodEnd,
		UsageStartAt:                   usageStart,
		UsageEndAt:                     usageEnd,
		BillableSeconds:                billableSeconds,
		ClaimCount:                     claimCount,
		RequestedCPUMillicores:         cloneOptionalInt64(fact.RequestedCPUMillicores),
		RequestedMemoryBytes:           cloneOptionalInt64(fact.RequestedMemoryBytes),
		RequestedEphemeralStorageBytes: cloneOptionalInt64(fact.RequestedEphemeralStorageBytes),
		RateMicros:                     rateMicros,
		AmountMicros:                   amountMicros,
		CreatedAt:                      createdAt,
	}
	row.ID = deterministicSharedAllocationSliceID(row)
	return row
}

func sharedAllocationClaimCount(allocationKind string) int64 {
	if allocationKind == SharedAllocationKindTenantClaim {
		return 1
	}
	return 0
}

func sharedClaimInsideUsageWindow(
	claimedAt time.Time,
	usageStart time.Time,
	usageEnd time.Time,
	includeFinalEnd bool,
) bool {
	if claimedAt.Before(usageStart) || claimedAt.After(usageEnd) {
		return false
	}
	return claimedAt.Before(usageEnd) || (includeFinalEnd && claimedAt.Equal(usageEnd))
}

func deterministicSharedAllocationRunID(run persistence.BillingSharedCostAllocationRun) uuid.UUID {
	return uuid.NewSHA1(deterministicNamespace, []byte(strings.Join([]string{
		"shared-cost-allocation-run",
		run.WorkerID.String(),
		fmt.Sprintf("%d", run.WorkerIncarnation),
		run.Provider,
		run.CurrencyCode,
		run.BillingPeriodStartAt.UTC().Format(time.RFC3339Nano),
		run.BillingPeriodEndAt.UTC().Format(time.RFC3339Nano),
		run.AlgorithmVersion,
	}, "\x00")))
}

func deterministicSharedAllocationSliceID(row persistence.BillingSharedEstimatedChargeSlice) uuid.UUID {
	return uuid.NewSHA1(deterministicNamespace, []byte(strings.Join([]string{
		"shared-cost-allocation-slice",
		row.RunID.String(),
		row.AllocationKind,
		sharedOptionalUUIDString(row.TenantID),
		sharedOptionalUUIDString(row.ClaimFactID),
		row.TariffID.String(),
		row.ChargeKind,
		row.UsageStartAt.UTC().Format(time.RFC3339Nano),
		row.UsageEndAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
}

func loadSharedAllocationRun(
	ctx context.Context,
	tx *gorm.DB,
	workerID uuid.UUID,
	workerIncarnation int64,
	provider string,
	currency string,
	periodStart time.Time,
	periodEnd time.Time,
) (persistence.BillingSharedCostAllocationRun, bool, error) {
	var run persistence.BillingSharedCostAllocationRun
	err := tx.WithContext(ctx).
		Where(
			"worker_id = ? AND worker_incarnation = ? AND provider = ? AND currency_code = ? AND billing_period_start_at = ? AND billing_period_end_at = ? AND algorithm_version = ?",
			workerID,
			workerIncarnation,
			provider,
			currency,
			periodStart,
			periodEnd,
			SharedAllocationAlgorithmClosedClaimIntervalV1,
		).
		Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.BillingSharedCostAllocationRun{}, false, nil
	}
	if err != nil {
		return persistence.BillingSharedCostAllocationRun{}, false, problem.Wrap(
			500,
			"cost_accounting_shared_allocation_load_failed",
			"The shared billing allocation run could not be loaded.",
			err,
		)
	}
	return run, true, nil
}

func loadSharedAllocationSlices(
	ctx context.Context,
	tx *gorm.DB,
	runID uuid.UUID,
) ([]persistence.BillingSharedEstimatedChargeSlice, error) {
	slices := make([]persistence.BillingSharedEstimatedChargeSlice, 0)
	if err := tx.WithContext(ctx).
		Where("run_id = ?", runID).
		Find(&slices).Error; err != nil {
		return nil, problem.Wrap(
			500,
			"cost_accounting_shared_allocation_slice_load_failed",
			"The shared billing allocation slices could not be loaded.",
			err,
		)
	}
	sortSharedAllocationSlices(slices)
	return slices, nil
}

func sortSharedAllocationSlices(slices []persistence.BillingSharedEstimatedChargeSlice) {
	sort.Slice(slices, func(i, j int) bool {
		if !slices[i].UsageStartAt.Equal(slices[j].UsageStartAt) {
			return slices[i].UsageStartAt.Before(slices[j].UsageStartAt)
		}
		if !slices[i].UsageEndAt.Equal(slices[j].UsageEndAt) {
			return slices[i].UsageEndAt.Before(slices[j].UsageEndAt)
		}
		if slices[i].ChargeKind != slices[j].ChargeKind {
			return chargeKindOrder(slices[i].ChargeKind) < chargeKindOrder(slices[j].ChargeKind)
		}
		return slices[i].ID.String() < slices[j].ID.String()
	})
}

func sameSharedAllocationRun(
	left persistence.BillingSharedCostAllocationRun,
	right persistence.BillingSharedCostAllocationRun,
) bool {
	return left.ID == right.ID &&
		left.ExecutionTargetID == right.ExecutionTargetID &&
		left.WorkerID == right.WorkerID &&
		left.WorkerIncarnation == right.WorkerIncarnation &&
		left.LedgerCoverageID == right.LedgerCoverageID &&
		left.Provider == right.Provider &&
		left.Region == right.Region &&
		left.CurrencyCode == right.CurrencyCode &&
		left.BillingPeriodStartAt.Equal(right.BillingPeriodStartAt) &&
		left.BillingPeriodEndAt.Equal(right.BillingPeriodEndAt) &&
		left.AlgorithmVersion == right.AlgorithmVersion &&
		left.UsageStartAt.Equal(right.UsageStartAt) &&
		left.UsageEndAt.Equal(right.UsageEndAt) &&
		left.ClaimCount == right.ClaimCount &&
		left.ReleaseCount == right.ReleaseCount &&
		left.LedgerSHA256 == right.LedgerSHA256 &&
		left.TenantAllocatedSeconds == right.TenantAllocatedSeconds &&
		left.PlatformIdleSeconds == right.PlatformIdleSeconds
}

func sameSharedAllocationSlices(
	left []persistence.BillingSharedEstimatedChargeSlice,
	right []persistence.BillingSharedEstimatedChargeSlice,
) bool {
	if len(left) != len(right) {
		return false
	}
	sortSharedAllocationSlices(left)
	sortSharedAllocationSlices(right)
	for index := range left {
		if !sameSharedAllocationSlice(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameSharedAllocationSlice(
	left persistence.BillingSharedEstimatedChargeSlice,
	right persistence.BillingSharedEstimatedChargeSlice,
) bool {
	return left.ID == right.ID &&
		left.RunID == right.RunID &&
		sameOptionalUUID(left.TenantID, right.TenantID) &&
		sameOptionalUUID(left.ClaimFactID, right.ClaimFactID) &&
		left.TariffID == right.TariffID &&
		left.ChargeKind == right.ChargeKind &&
		left.AllocationKind == right.AllocationKind &&
		left.ResourceCorrelationKey == right.ResourceCorrelationKey &&
		left.BillingPeriodStartAt.Equal(right.BillingPeriodStartAt) &&
		left.BillingPeriodEndAt.Equal(right.BillingPeriodEndAt) &&
		left.UsageStartAt.Equal(right.UsageStartAt) &&
		left.UsageEndAt.Equal(right.UsageEndAt) &&
		left.BillableSeconds == right.BillableSeconds &&
		left.ClaimCount == right.ClaimCount &&
		sameOptionalInt64(left.RequestedCPUMillicores, right.RequestedCPUMillicores) &&
		sameOptionalInt64(left.RequestedMemoryBytes, right.RequestedMemoryBytes) &&
		sameOptionalInt64(left.RequestedEphemeralStorageBytes, right.RequestedEphemeralStorageBytes) &&
		left.RateMicros == right.RateMicros &&
		left.AmountMicros == right.AmountMicros
}

func sharedOptionalUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func sharedOptionalInt64String(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func sharedOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cloneOptionalUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
