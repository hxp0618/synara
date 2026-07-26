package sessions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	sharedrecoverybundle "github.com/synara-ai/synara/services/control-plane/internal/recoverybundle"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

type sourceDRRoutingAuthority struct {
	SourceRegion        string
	SourceClusterID     string
	SourceDRDomain      string
	ReplicatedThroughAt time.Time
	RequiredStores      routing.DRStoreRequirements
}

type failoverBundleAuthorityPayload struct {
	Execution struct {
		SelectedRegion      *string    `json:"selectedRegion"`
		SelectedClusterID   *string    `json:"selectedClusterId"`
		RestoreCheckpointID *uuid.UUID `json:"restoreCheckpointId"`
	} `json:"execution"`
	Workload struct {
		ResumeSnapshot struct {
			ArtifactReferences []failoverBundleArtifactReference `json:"artifactReferences"`
		} `json:"resumeSnapshot"`
		MemoryReferences []struct {
			ArtifactID uuid.UUID `json:"artifactId"`
		} `json:"memoryReferences"`
	} `json:"workload"`
}

const drRoutingResumeEventLimit = 500

var drRoutingResumeEventTypes = []string{
	"turn.created",
	"turn.steer-requested",
	"runtime.output.delta",
	"content.delta",
	"tool.summary",
	"item.started",
	"item.updated",
	"item.completed",
	"thread.state.changed",
	"artifact.ready",
}

type artifactAuthorityExpectation struct {
	Sequence    int64
	ArtifactID  uuid.UUID
	SessionID   uuid.UUID
	ExecutionID *uuid.UUID
}

type failoverBundleArtifactReference struct {
	Sequence    int64      `json:"sequence"`
	ArtifactID  uuid.UUID  `json:"artifactId"`
	ExecutionID *uuid.UUID `json:"executionId"`
}

func loadSessionDRRoutingAuthority(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
) (sourceDRRoutingAuthority, bool, error) {
	latestExecution, found, err := loadLatestSessionExecution(ctx, tx, session.TenantID, session.ID)
	if err != nil {
		return sourceDRRoutingAuthority{}, false, err
	}
	if !found {
		return sourceDRRoutingAuthority{}, false, nil
	}
	authority, err := loadSessionSourceDomain(latestExecution)
	if err != nil {
		return sourceDRRoutingAuthority{}, false, err
	}
	if err := enrichSessionDRBackingRequirements(ctx, tx, session, &authority); err != nil {
		return sourceDRRoutingAuthority{}, false, err
	}
	if !authority.RequiredStores.Any() {
		authority.ReplicatedThroughAt = time.Time{}
	}
	return authority, true, nil
}

func BuildSessionExecutionTargetGroupSelectRequest(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
) (routing.SelectRequest, error) {
	if session.ExecutionTargetGroupID == nil {
		return routing.SelectRequest{}, problem.New(
			409,
			"session_target_group_required",
			"The Session is not configured for global target routing.",
		)
	}
	preferredRegions := make([]string, 0, 1)
	if session.PreferredExecutionRegion != nil {
		preferredRegions = append(preferredRegions, *session.PreferredExecutionRegion)
	}
	preferredTarget := session.ExecutionTargetID
	sourceAuthority, hasSourceAuthority, authorityErr := loadSessionDRRoutingAuthority(ctx, tx, session)
	if authorityErr != nil {
		return routing.SelectRequest{}, authorityErr
	}

	selectRequest := routing.SelectRequest{
		TenantID:          session.TenantID,
		OrganizationID:    session.OrganizationID,
		TargetGroupID:     *session.ExecutionTargetGroupID,
		Provider:          session.Provider,
		PreferredTargetID: &preferredTarget,
		PreferredRegions:  preferredRegions,
	}
	if hasSourceAuthority {
		selectRequest.SourceRegion = sourceAuthority.SourceRegion
		selectRequest.SourceClusterID = sourceAuthority.SourceClusterID
		selectRequest.SourceDRDomain = sourceAuthority.SourceDRDomain
		selectRequest.ReplicatedThroughAt = sourceAuthority.ReplicatedThroughAt
		selectRequest.RequiredDRStores = sourceAuthority.RequiredStores
	}
	return selectRequest, nil
}

func AdvanceSessionExecutionTargetAuthority(
	ctx context.Context,
	tx *gorm.DB,
	session *persistence.AgentSession,
	selection routing.Selection,
	updatedAt time.Time,
) error {
	if session == nil {
		return problem.New(500, "session_target_route_update_failed", "The Session routing authority could not be advanced.")
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	if session.ExecutionTargetID == selection.Target.ID && session.RoutingPolicyVersion != nil &&
		*session.RoutingPolicyVersion == selection.Group.Version {
		return nil
	}
	if err := tx.WithContext(ctx).Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", session.TenantID, session.ID).
		Updates(map[string]any{
			"execution_target_id":    selection.Target.ID,
			"routing_policy_version": selection.Group.Version,
			"updated_at":             updatedAt.UTC(),
		}).Error; err != nil {
		return problem.Wrap(
			409,
			"session_target_route_update_failed",
			"The Session routing authority could not be advanced.",
			err,
		)
	}
	session.ExecutionTargetID = selection.Target.ID
	version := selection.Group.Version
	session.RoutingPolicyVersion = &version
	return nil
}

func loadLatestSessionExecution(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
) (persistence.AgentExecution, bool, error) {
	var execution persistence.AgentExecution
	err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		Order("queued_at DESC, attempt DESC, id DESC").
		Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.AgentExecution{}, false, nil
	}
	if err != nil {
		return persistence.AgentExecution{}, false, problem.Wrap(
			500,
			"session_routing_source_load_failed",
			"The latest Session execution could not be loaded for cross-domain routing.",
			err,
		)
	}
	return execution, true, nil
}

func loadSessionSourceDomain(latestExecution persistence.AgentExecution) (sourceDRRoutingAuthority, error) {
	authority := sourceDRRoutingAuthority{
		SourceRegion:    valueOrEmpty(latestExecution.SelectedRegion),
		SourceClusterID: valueOrEmpty(latestExecution.SelectedClusterID),
	}
	if authority.SourceRegion == "" || authority.SourceClusterID == "" {
		return sourceDRRoutingAuthority{}, problem.New(
			409,
			"session_routing_source_domain_missing",
			"The latest Session execution is missing its immutable region and cluster routing snapshot.",
		)
	}
	if authority.SourceRegion != "" && authority.SourceClusterID != "" {
		authority.SourceDRDomain = routing.DRDomainForLocation(authority.SourceRegion, authority.SourceClusterID)
	}
	return authority, nil
}

func enrichSessionDRBackingRequirements(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
	authority *sourceDRRoutingAuthority,
) error {
	var workspace persistence.RemoteWorkspace
	err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", session.TenantID, session.ID).
		Take(&workspace).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Wrap(
			500,
			"session_routing_workspace_load_failed",
			"The current Session workspace could not be loaded for cross-domain routing.",
			err,
		)
	}
	if err == nil {
		checkpointWatermark, needsCheckpoint, checkpointErr := loadSessionCheckpointAuthority(ctx, tx, workspace)
		if checkpointErr != nil {
			return checkpointErr
		}
		if needsCheckpoint {
			authority.RequiredStores.Checkpoints = true
			authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, checkpointWatermark)
		}
	}
	artifactWatermark, needsArtifacts, artifactErr := loadSessionResumeArtifactAuthority(ctx, tx, session)
	if artifactErr != nil {
		return artifactErr
	}
	if needsArtifacts {
		authority.RequiredStores.Artifacts = true
		authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, artifactWatermark)
	}
	memoryWatermark, needsMemory, memoryErr := loadSessionMemoryAuthority(ctx, tx, session)
	if memoryErr != nil {
		return memoryErr
	}
	if needsMemory {
		authority.RequiredStores.Memory = true
		authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, memoryWatermark)
	}
	return nil
}

func loadSessionCheckpointAuthority(
	ctx context.Context,
	tx *gorm.DB,
	workspace persistence.RemoteWorkspace,
) (time.Time, bool, error) {
	if workspace.CurrentCheckpointID == nil && workspace.State != "dirty" {
		return time.Time{}, false, nil
	}
	if workspace.CurrentCheckpointID == nil {
		return time.Time{}, true, nil
	}
	var checkpoint persistence.WorkspaceCheckpoint
	err := tx.WithContext(ctx).
		Select("ready_at").
		Where(
			"tenant_id = ? AND workspace_id = ? AND session_id = ? AND id = ? AND status = ? AND ready_at IS NOT NULL",
			workspace.TenantID, workspace.ID, workspace.SessionID, *workspace.CurrentCheckpointID, "ready",
		).
		Take(&checkpoint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, true, nil
	}
	if err != nil {
		return time.Time{}, false, problem.Wrap(
			500,
			"session_routing_checkpoint_load_failed",
			"The current Session workspace checkpoint could not be loaded for cross-domain routing.",
			err,
		)
	}
	if checkpoint.ReadyAt == nil {
		return time.Time{}, true, nil
	}
	return checkpoint.ReadyAt.UTC(), true, nil
}

func loadSessionMemoryAuthority(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
) (time.Time, bool, error) {
	artifacts, err := loadEffectiveSessionMemoryArtifacts(ctx, tx, session)
	if err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) && apiError.Status < 500 {
			return time.Time{}, true, nil
		}
		return time.Time{}, false, err
	}
	if len(artifacts) == 0 {
		return time.Time{}, false, nil
	}
	watermark := time.Time{}
	for _, artifact := range artifacts {
		watermark = maxTime(watermark, artifact.ReadyAt)
	}
	return watermark, true, nil
}

func loadSessionResumeArtifactAuthority(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
) (time.Time, bool, error) {
	if session.LastEventSequence <= 0 {
		return time.Time{}, false, nil
	}
	expectations, err := loadResumeArtifactExpectations(
		ctx,
		tx,
		session.TenantID,
		session.ID,
		session.LastEventSequence,
	)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(expectations) == 0 {
		return time.Time{}, false, nil
	}
	watermark, err := loadReadyArtifactAuthorityWatermark(
		ctx,
		tx,
		session.TenantID,
		expectations,
		"session_routing_artifact_authority_load_failed",
		"The current Session Artifact recovery authority could not be loaded for cross-domain routing.",
		"session_routing_artifact_authority_missing",
		"The current Session Artifact recovery authority is missing or no longer ready for cross-domain routing.",
	)
	if err != nil {
		return time.Time{}, false, err
	}
	return watermark, true, nil
}

type sessionMemoryArtifactAuthority struct {
	ArtifactID uuid.UUID
	ReadyAt    time.Time
}

func loadEffectiveSessionMemoryArtifacts(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
) ([]sessionMemoryArtifactAuthority, error) {
	var heads []persistence.AgentMemoryHead
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND enabled = ?", session.TenantID, true).
		Where(
			"(scope_type = 'project' AND scope_project_id = ?) OR (scope_type = 'session' AND scope_session_id = ?)",
			session.ProjectID, session.ID,
		).
		Order("scope_type, memory_key, id").
		Find(&heads).Error; err != nil {
		return nil, problem.Wrap(
			500,
			"session_routing_memory_heads_load_failed",
			"The current Session memory heads could not be loaded for cross-domain routing.",
			err,
		)
	}
	if len(heads) == 0 {
		return nil, nil
	}

	winners := make(map[string]persistence.AgentMemoryHead, len(heads))
	for _, head := range heads {
		current, found := winners[head.MemoryKey]
		if !found || sessionMemoryScopePriority(head.ScopeType) > sessionMemoryScopePriority(current.ScopeType) {
			winners[head.MemoryKey] = head
		}
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	artifacts := make([]sessionMemoryArtifactAuthority, 0, len(keys))
	seenArtifacts := make(map[uuid.UUID]struct{}, len(keys))
	for _, key := range keys {
		head := winners[key]
		artifact, err := loadSessionMemoryArtifactAuthority(ctx, tx, session, head)
		if err != nil {
			return nil, err
		}
		if _, exists := seenArtifacts[artifact.ArtifactID]; exists {
			continue
		}
		seenArtifacts[artifact.ArtifactID] = struct{}{}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func loadSessionMemoryArtifactAuthority(
	ctx context.Context,
	tx *gorm.DB,
	session persistence.AgentSession,
	head persistence.AgentMemoryHead,
) (sessionMemoryArtifactAuthority, error) {
	if head.CurrentRevisionID == nil || head.Version <= 0 {
		return sessionMemoryArtifactAuthority{}, problem.New(
			409,
			"memory_head_invalid",
			"An enabled Agent Memory Head has no published Revision.",
		)
	}

	var revision persistence.AgentMemoryRevision
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND memory_head_id = ? AND id = ?", session.TenantID, head.ID, *head.CurrentRevisionID).
		Take(&revision).Error; err != nil {
		return sessionMemoryArtifactAuthority{}, problem.Wrap(
			409,
			"memory_revision_unavailable",
			"The immutable Agent Memory Revision is unavailable.",
			err,
		)
	}
	if revision.RevisionNumber != head.Version {
		return sessionMemoryArtifactAuthority{}, problem.New(
			409,
			"memory_revision_mismatch",
			"The Agent Memory Head and immutable Revision version do not match.",
		)
	}

	var artifact persistence.Artifact
	if err := tx.WithContext(ctx).
		Select("id", "project_id", "session_id", "ready_at").
		Where(
			"tenant_id = ? AND id = ? AND kind = ? AND status = ? AND deleted_at IS NULL AND sha256 = ? AND ready_at IS NOT NULL",
			session.TenantID, revision.ArtifactID, "memory", "ready", revision.SHA256,
		).
		Take(&artifact).Error; err != nil {
		return sessionMemoryArtifactAuthority{}, problem.Wrap(
			409,
			"memory_artifact_unavailable",
			"The Memory Artifact is unavailable or does not match its immutable SHA-256.",
			err,
		)
	}
	switch head.ScopeType {
	case "project":
		if artifact.ProjectID != session.ProjectID {
			return sessionMemoryArtifactAuthority{}, problem.New(
				409,
				"memory_artifact_scope_mismatch",
				"The Memory Artifact does not belong to its Project scope.",
			)
		}
	case "session":
		if artifact.SessionID != session.ID {
			return sessionMemoryArtifactAuthority{}, problem.New(
				409,
				"memory_artifact_scope_mismatch",
				"The Memory Artifact does not belong to its Session scope.",
			)
		}
	default:
		return sessionMemoryArtifactAuthority{}, problem.New(
			409,
			"memory_scope_invalid",
			"The Memory Head scope is invalid.",
		)
	}
	if artifact.ReadyAt == nil {
		return sessionMemoryArtifactAuthority{}, problem.New(
			409,
			"memory_artifact_unavailable",
			"The Memory Artifact is unavailable or does not match its immutable SHA-256.",
		)
	}
	return sessionMemoryArtifactAuthority{
		ArtifactID: artifact.ID,
		ReadyAt:    artifact.ReadyAt.UTC(),
	}, nil
}

func sessionMemoryScopePriority(scope string) int {
	switch scope {
	case "session":
		return 2
	case "project":
		return 1
	default:
		return 0
	}
}

func loadFailoverReplicationAuthority(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	session persistence.AgentSession,
	source persistence.AgentExecution,
	sourceBundle *persistence.ExecutionRecoveryBundle,
) (sourceDRRoutingAuthority, error) {
	authority := sourceDRRoutingAuthority{
		SourceRegion:    valueOrEmpty(source.SelectedRegion),
		SourceClusterID: valueOrEmpty(source.SelectedClusterID),
	}
	var payload failoverBundleAuthorityPayload
	if sourceBundle != nil {
		err := sharedrecoverybundle.DecodeAndValidate(*sourceBundle, &payload)
		switch {
		case errors.Is(err, sharedrecoverybundle.ErrIntegrityFailed):
			return sourceDRRoutingAuthority{}, problem.New(
				409,
				"target_failover_bundle_integrity_failed",
				"The source Recovery Bundle payload hash does not match its immutable envelope.",
			)
		case errors.Is(err, sharedrecoverybundle.ErrEnvelopeMismatch):
			return sourceDRRoutingAuthority{}, problem.New(
				409,
				"target_failover_bundle_envelope_mismatch",
				"The source Recovery Bundle payload does not match its immutable execution envelope.",
			)
		case err != nil:
			return sourceDRRoutingAuthority{}, problem.Wrap(
				409,
				"target_failover_bundle_authority_invalid",
				"The source Recovery Bundle could not be validated for DR authority.",
				err,
			)
		}
		if authority.SourceRegion == "" {
			authority.SourceRegion = valueOrEmpty(payload.Execution.SelectedRegion)
		}
		if authority.SourceClusterID == "" {
			authority.SourceClusterID = valueOrEmpty(payload.Execution.SelectedClusterID)
		}
	}
	if authority.SourceRegion == "" || authority.SourceClusterID == "" {
		return sourceDRRoutingAuthority{}, problem.New(
			409,
			"target_failover_source_domain_missing",
			"The source Execution is missing its immutable DR domain snapshot.",
		)
	}
	authority.SourceDRDomain = routing.DRDomainForLocation(authority.SourceRegion, authority.SourceClusterID)
	if payload.Execution.RestoreCheckpointID != nil {
		authority.RequiredStores.Checkpoints = true
		var checkpoint persistence.WorkspaceCheckpoint
		if err := tx.WithContext(ctx).
			Where(
				"tenant_id = ? AND id = ? AND status = ? AND ready_at IS NOT NULL",
				tenantID, *payload.Execution.RestoreCheckpointID, "ready",
			).
			Take(&checkpoint).Error; err != nil {
			return sourceDRRoutingAuthority{}, problem.Wrap(
				409,
				"target_failover_checkpoint_authority_missing",
				"The source Recovery Bundle references a Workspace Checkpoint without ready authority.",
				err,
			)
		}
		if checkpoint.ReadyAt != nil {
			authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, checkpoint.ReadyAt.UTC())
		}
	}
	throughSequence := session.LastEventSequence
	if sourceBundle != nil && sourceBundle.AuthoritativeHistorySequence > throughSequence {
		throughSequence = sourceBundle.AuthoritativeHistorySequence
	}
	artifactExpectations, err := loadResumeArtifactExpectations(
		ctx,
		tx,
		tenantID,
		source.SessionID,
		throughSequence,
	)
	if err != nil {
		return sourceDRRoutingAuthority{}, problem.Wrap(
			500,
			"target_failover_artifact_authority_load_failed",
			"Source Result Artifact authority could not be loaded.",
			err,
		)
	}
	bundleThroughSequence := throughSequence
	if sourceBundle != nil {
		bundleThroughSequence = sourceBundle.AuthoritativeHistorySequence
	}
	bundleExpectations, err := bundleArtifactAuthorityExpectations(
		ctx,
		tx,
		tenantID,
		source.SessionID,
		bundleThroughSequence,
		payload.Workload.ResumeSnapshot.ArtifactReferences,
	)
	if err != nil {
		return sourceDRRoutingAuthority{}, err
	}
	artifactExpectations, err = mergeArtifactAuthorityExpectations(artifactExpectations, bundleExpectations)
	if err != nil {
		return sourceDRRoutingAuthority{}, err
	}
	if len(artifactExpectations) != 0 {
		authority.RequiredStores.Artifacts = true
		watermark, err := loadReadyArtifactAuthorityWatermark(
			ctx,
			tx,
			tenantID,
			artifactExpectations,
			"target_failover_artifact_authority_load_failed",
			"Source Result Artifact authority could not be loaded.",
			"target_failover_artifact_authority_missing",
			"The source Recovery Bundle references a result Artifact without ready authority.",
		)
		if err != nil {
			return sourceDRRoutingAuthority{}, err
		}
		authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, watermark)
	}
	if len(payload.Workload.MemoryReferences) != 0 {
		authority.RequiredStores.Memory = true
		artifactIDs := make([]uuid.UUID, 0, len(payload.Workload.MemoryReferences))
		for _, reference := range payload.Workload.MemoryReferences {
			artifactIDs = append(artifactIDs, reference.ArtifactID)
		}
		watermark, err := loadReadyArtifactIDsWatermark(
			ctx,
			tx,
			tenantID,
			artifactIDs,
			"target_failover_memory_authority_load_failed",
			"Source Memory Artifact authority could not be loaded.",
			"target_failover_memory_authority_missing",
			"The source Recovery Bundle references a Memory Artifact without ready authority.",
		)
		if err != nil {
			return sourceDRRoutingAuthority{}, err
		}
		authority.ReplicatedThroughAt = maxTime(authority.ReplicatedThroughAt, watermark)
	}
	if !authority.RequiredStores.Any() {
		authority.ReplicatedThroughAt = time.Time{}
	}
	return authority, nil
}

func loadReadyArtifactIDsWatermark(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	artifactIDs []uuid.UUID,
	loadCode, loadMessage, missingCode, missingMessage string,
) (time.Time, error) {
	artifactIDs = dedupeArtifactIDs(artifactIDs)
	if len(artifactIDs) == 0 {
		return time.Time{}, nil
	}
	expectations := make([]artifactAuthorityExpectation, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		expectations = append(expectations, artifactAuthorityExpectation{ArtifactID: artifactID})
	}
	return loadReadyArtifactAuthorityWatermark(ctx, tx, tenantID, expectations, loadCode, loadMessage, missingCode, missingMessage)
}

func loadReadyArtifactAuthorityWatermark(
	ctx context.Context,
	tx *gorm.DB,
	tenantID uuid.UUID,
	expectations []artifactAuthorityExpectation,
	loadCode, loadMessage, missingCode, missingMessage string,
) (time.Time, error) {
	expectations = dedupeArtifactAuthorityExpectations(expectations)
	if len(expectations) == 0 {
		return time.Time{}, nil
	}
	artifactIDs := make([]uuid.UUID, 0, len(expectations))
	for _, expectation := range expectations {
		artifactIDs = append(artifactIDs, expectation.ArtifactID)
	}
	var artifacts []persistence.Artifact
	if err := tx.WithContext(ctx).
		Where(
			"tenant_id = ? AND id IN ? AND status = ? AND deleted_at IS NULL AND ready_at IS NOT NULL",
			tenantID, artifactIDs, "ready",
		).
		Find(&artifacts).Error; err != nil {
		return time.Time{}, problem.Wrap(500, loadCode, loadMessage, err)
	}
	if len(artifacts) != len(expectations) {
		return time.Time{}, problem.New(409, missingCode, missingMessage)
	}
	byID := make(map[uuid.UUID]persistence.Artifact, len(artifacts))
	for _, artifact := range artifacts {
		byID[artifact.ID] = artifact
	}
	watermark := time.Time{}
	for _, expectation := range expectations {
		artifact, ok := byID[expectation.ArtifactID]
		if !ok || artifact.ReadyAt == nil {
			return time.Time{}, problem.New(409, missingCode, missingMessage)
		}
		if expectation.SessionID != uuid.Nil && artifact.SessionID != expectation.SessionID {
			return time.Time{}, problem.New(409, missingCode, missingMessage)
		}
		if expectation.ExecutionID != nil {
			if artifact.ExecutionID == nil || *artifact.ExecutionID != *expectation.ExecutionID {
				return time.Time{}, problem.New(409, missingCode, missingMessage)
			}
		}
		if artifact.ReadyAt != nil {
			watermark = maxTime(watermark, artifact.ReadyAt.UTC())
		}
	}
	return watermark, nil
}

func loadResumeArtifactExpectations(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence int64,
) ([]artifactAuthorityExpectation, error) {
	logical, err := loadEffectiveResumeAuthorityEvents(ctx, tx, tenantID, sessionID, throughSequence)
	if err != nil {
		return nil, err
	}
	return projectResumeArtifactExpectations(logical), nil
}

func loadEffectiveResumeAuthorityEvents(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence int64,
) ([]LogicalEvent, error) {
	if throughSequence <= 0 {
		return []LogicalEvent{}, nil
	}
	remaining := drRoutingResumeEventLimit + 1
	cursor := throughSequence
	chunksNewestFirst := make([][]LogicalEvent, 0, 4)
	for cursor > 0 && remaining > 0 {
		rollbacks, err := LoadLogicalEventsTail(
			ctx,
			tx,
			tenantID,
			sessionID,
			0,
			cursor,
			1,
			[]string{"session.history.rolled-back"},
		)
		if err != nil {
			return nil, err
		}
		lowerBound := int64(0)
		if len(rollbacks) > 0 {
			lowerBound = rollbacks[0].Event.Sequence
		}
		logical, err := LoadLogicalEventsTail(
			ctx,
			tx,
			tenantID,
			sessionID,
			lowerBound,
			cursor,
			remaining,
			drRoutingResumeEventTypes,
		)
		if err != nil {
			return nil, err
		}
		chunk := make([]LogicalEvent, 0, len(logical))
		chunk = append(chunk, logical...)
		chunksNewestFirst = append(chunksNewestFirst, chunk)
		remaining -= len(chunk)
		if len(rollbacks) == 0 {
			break
		}
		fromSequence, ok := safeArtifactAuthorityInt64Payload(rollbacks[0].Event.Payload, "fromSequence")
		if !ok || fromSequence <= 0 || fromSequence >= rollbacks[0].Event.Sequence {
			return nil, problem.New(409, "rollback_target_stale", "The rollback event contains an invalid source sequence.")
		}
		cursor = fromSequence - 1
	}
	combined := make([]LogicalEvent, 0, drRoutingResumeEventLimit+1-remaining)
	for index := len(chunksNewestFirst) - 1; index >= 0; index-- {
		combined = append(combined, chunksNewestFirst[index]...)
	}
	if len(combined) > drRoutingResumeEventLimit {
		combined = combined[len(combined)-drRoutingResumeEventLimit:]
	}
	return combined, nil
}

func projectResumeArtifactExpectations(events []LogicalEvent) []artifactAuthorityExpectation {
	ordered := append([]LogicalEvent(nil), events...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Event.Sequence == ordered[right].Event.Sequence {
			return ordered[left].Event.EventID.String() < ordered[right].Event.EventID.String()
		}
		return ordered[left].Event.Sequence < ordered[right].Event.Sequence
	})
	expectations := make([]artifactAuthorityExpectation, 0)
	seenArtifacts := make(map[uuid.UUID]struct{})
	compactBoundary := int64(0)
	for _, logical := range ordered {
		switch logical.Event.EventType {
		case "thread.state.changed":
			if safeArtifactAuthorityStringPayload(logical.Event.Payload, "state") == "compacted" {
				compactBoundary = logical.Event.Sequence
				if sequence, ok := safeArtifactAuthorityInt64Payload(logical.Event.Payload, "compactedThroughSequence"); ok {
					compactBoundary = sequence
				}
			}
		case "item.started", "item.updated", "item.completed":
			if safeArtifactAuthorityStringPayload(logical.Event.Payload, "itemType") != "context_compaction" {
				continue
			}
			if logical.Event.EventType == "item.completed" ||
				safeArtifactAuthorityStringPayload(logical.Event.Payload, "status") == "completed" {
				compactBoundary = logical.Event.Sequence
			}
		case "artifact.ready":
			artifactID, ok := safeArtifactAuthorityUUIDPayload(logical.Event.Payload, "artifactId")
			if !ok {
				continue
			}
			if _, exists := seenArtifacts[artifactID]; exists {
				continue
			}
			seenArtifacts[artifactID] = struct{}{}
			expectation := artifactAuthorityExpectation{
				Sequence:   logical.Event.Sequence,
				ArtifactID: artifactID,
				SessionID:  logical.OriginSessionID,
			}
			if logical.Event.ExecutionID != nil {
				executionID := *logical.Event.ExecutionID
				expectation.ExecutionID = &executionID
			}
			expectations = append(expectations, expectation)
		}
	}
	if compactBoundary <= 0 {
		return expectations
	}
	filtered := expectations[:0]
	for _, expectation := range expectations {
		if expectation.Sequence > compactBoundary {
			filtered = append(filtered, expectation)
		}
	}
	return filtered
}

func dedupeArtifactIDs(artifactIDs []uuid.UUID) []uuid.UUID {
	if len(artifactIDs) <= 1 {
		return artifactIDs
	}
	result := make([]uuid.UUID, 0, len(artifactIDs))
	seen := make(map[uuid.UUID]struct{}, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		if _, exists := seen[artifactID]; exists {
			continue
		}
		seen[artifactID] = struct{}{}
		result = append(result, artifactID)
	}
	return result
}

func dedupeArtifactAuthorityExpectations(expectations []artifactAuthorityExpectation) []artifactAuthorityExpectation {
	if len(expectations) <= 1 {
		return expectations
	}
	result := make([]artifactAuthorityExpectation, 0, len(expectations))
	seen := make(map[uuid.UUID]struct{}, len(expectations))
	for _, expectation := range expectations {
		if expectation.ArtifactID == uuid.Nil {
			continue
		}
		if _, exists := seen[expectation.ArtifactID]; exists {
			continue
		}
		seen[expectation.ArtifactID] = struct{}{}
		result = append(result, expectation)
	}
	return result
}

func mergeArtifactAuthorityExpectations(
	left, right []artifactAuthorityExpectation,
) ([]artifactAuthorityExpectation, error) {
	if len(left) == 0 {
		return coalesceArtifactAuthorityExpectations(
			right,
			"target_failover_bundle_authority_invalid",
			"The source Recovery Bundle contains conflicting result Artifact authority.",
		)
	}
	if len(right) == 0 {
		return coalesceArtifactAuthorityExpectations(
			left,
			"target_failover_bundle_authority_invalid",
			"The source Recovery Bundle contains conflicting result Artifact authority.",
		)
	}
	merged := make([]artifactAuthorityExpectation, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	return coalesceArtifactAuthorityExpectations(
		merged,
		"target_failover_bundle_authority_invalid",
		"The source Recovery Bundle contains conflicting result Artifact authority.",
	)
}

func bundleArtifactAuthorityExpectations(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence int64,
	references []failoverBundleArtifactReference,
) ([]artifactAuthorityExpectation, error) {
	if len(references) == 0 {
		return nil, nil
	}
	bySequence := make(map[int64]artifactAuthorityExpectation, len(references))
	expectations := make([]artifactAuthorityExpectation, 0, len(references))
	for _, reference := range references {
		if reference.ArtifactID == uuid.Nil || reference.Sequence <= 0 || reference.Sequence > throughSequence {
			return nil, problem.New(
				409,
				"target_failover_bundle_authority_invalid",
				"The source Recovery Bundle contains an invalid result Artifact reference.",
			)
		}
		expectation, found := bySequence[reference.Sequence]
		if !found {
			var err error
			expectation, found, err = loadEffectiveArtifactAuthorityExpectationAtSequence(
				ctx,
				tx,
				tenantID,
				sessionID,
				throughSequence,
				reference.Sequence,
			)
			if err != nil {
				return nil, err
			}
			if !found || expectation.ArtifactID != reference.ArtifactID {
				return nil, problem.New(
					409,
					"target_failover_artifact_authority_missing",
					"The source Recovery Bundle references a result Artifact without authoritative logical history.",
				)
			}
			bySequence[reference.Sequence] = expectation
		}
		if reference.ExecutionID != nil {
			if expectation.ExecutionID != nil && *expectation.ExecutionID != *reference.ExecutionID {
				return nil, problem.New(
					409,
					"target_failover_bundle_authority_invalid",
					"The source Recovery Bundle contains conflicting result Artifact authority.",
				)
			}
			executionID := *reference.ExecutionID
			expectation.ExecutionID = &executionID
		}
		expectations = append(expectations, expectation)
	}
	return coalesceArtifactAuthorityExpectations(
		expectations,
		"target_failover_bundle_authority_invalid",
		"The source Recovery Bundle contains conflicting result Artifact authority.",
	)
}

func loadEffectiveArtifactAuthorityExpectationAtSequence(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence, sequence int64,
) (artifactAuthorityExpectation, bool, error) {
	if sequence <= 0 || throughSequence < sequence {
		return artifactAuthorityExpectation{}, false, nil
	}
	cursor := throughSequence
	for cursor >= sequence && cursor > 0 {
		rollbacks, err := LoadLogicalEventsTail(
			ctx,
			tx,
			tenantID,
			sessionID,
			0,
			cursor,
			1,
			[]string{"session.history.rolled-back"},
		)
		if err != nil {
			return artifactAuthorityExpectation{}, false, problem.Wrap(
				500,
				"target_failover_artifact_authority_load_failed",
				"Source Result Artifact authority could not be loaded.",
				err,
			)
		}
		if len(rollbacks) == 0 {
			return loadArtifactAuthorityExpectationAtSequence(ctx, tx, tenantID, sessionID, sequence)
		}
		rollback := rollbacks[0].Event
		fromSequence, ok := safeArtifactAuthorityInt64Payload(rollback.Payload, "fromSequence")
		if !ok || fromSequence <= 0 || fromSequence >= rollback.Sequence {
			return artifactAuthorityExpectation{}, false, problem.New(
				409,
				"rollback_target_stale",
				"The rollback event contains an invalid source sequence.",
			)
		}
		if sequence > rollback.Sequence {
			return loadArtifactAuthorityExpectationAtSequence(ctx, tx, tenantID, sessionID, sequence)
		}
		if sequence >= fromSequence && sequence < rollback.Sequence {
			return artifactAuthorityExpectation{}, false, nil
		}
		cursor = fromSequence - 1
	}
	return artifactAuthorityExpectation{}, false, nil
}

func loadArtifactAuthorityExpectationAtSequence(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	sequence int64,
) (artifactAuthorityExpectation, bool, error) {
	logical, err := LoadLogicalEventsPage(ctx, tx, tenantID, sessionID, sequence-1, sequence, 2)
	if err != nil {
		return artifactAuthorityExpectation{}, false, problem.Wrap(
			500,
			"target_failover_artifact_authority_load_failed",
			"Source Result Artifact authority could not be loaded.",
			err,
		)
	}
	if len(logical) != 1 || logical[0].Event.Sequence != sequence || logical[0].Event.EventType != "artifact.ready" {
		return artifactAuthorityExpectation{}, false, nil
	}
	artifactID, ok := safeArtifactAuthorityUUIDPayload(logical[0].Event.Payload, "artifactId")
	if !ok {
		return artifactAuthorityExpectation{}, false, nil
	}
	expectation := artifactAuthorityExpectation{
		Sequence:   sequence,
		ArtifactID: artifactID,
		SessionID:  logical[0].OriginSessionID,
	}
	if logical[0].Event.ExecutionID != nil {
		executionID := *logical[0].Event.ExecutionID
		expectation.ExecutionID = &executionID
	}
	return expectation, true, nil
}

func coalesceArtifactAuthorityExpectations(
	expectations []artifactAuthorityExpectation,
	conflictCode, conflictMessage string,
) ([]artifactAuthorityExpectation, error) {
	if len(expectations) <= 1 {
		return expectations, nil
	}
	order := make([]uuid.UUID, 0, len(expectations))
	byID := make(map[uuid.UUID]artifactAuthorityExpectation, len(expectations))
	for _, expectation := range expectations {
		if expectation.ArtifactID == uuid.Nil {
			continue
		}
		current, exists := byID[expectation.ArtifactID]
		if !exists {
			byID[expectation.ArtifactID] = expectation
			order = append(order, expectation.ArtifactID)
			continue
		}
		merged, err := combineArtifactAuthorityExpectation(current, expectation, conflictCode, conflictMessage)
		if err != nil {
			return nil, err
		}
		byID[expectation.ArtifactID] = merged
	}
	result := make([]artifactAuthorityExpectation, 0, len(order))
	for _, artifactID := range order {
		result = append(result, byID[artifactID])
	}
	return result, nil
}

func combineArtifactAuthorityExpectation(
	left, right artifactAuthorityExpectation,
	conflictCode, conflictMessage string,
) (artifactAuthorityExpectation, error) {
	merged := left
	if merged.Sequence > 0 && right.Sequence > 0 && merged.Sequence != right.Sequence {
		return artifactAuthorityExpectation{}, problem.New(409, conflictCode, conflictMessage)
	}
	if merged.Sequence == 0 {
		merged.Sequence = right.Sequence
	}
	if merged.SessionID != uuid.Nil && right.SessionID != uuid.Nil && merged.SessionID != right.SessionID {
		return artifactAuthorityExpectation{}, problem.New(409, conflictCode, conflictMessage)
	}
	if merged.SessionID == uuid.Nil {
		merged.SessionID = right.SessionID
	}
	if merged.ExecutionID != nil && right.ExecutionID != nil && *merged.ExecutionID != *right.ExecutionID {
		return artifactAuthorityExpectation{}, problem.New(409, conflictCode, conflictMessage)
	}
	if merged.ExecutionID == nil && right.ExecutionID != nil {
		executionID := *right.ExecutionID
		merged.ExecutionID = &executionID
	}
	return merged, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func maxTime(left, right time.Time) time.Time {
	if left.IsZero() || right.After(left) {
		return right
	}
	return left
}

func safeArtifactAuthorityInt64Payload(payload map[string]any, key string) (int64, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int64:
		return typed, typed >= 0
	case int:
		return int64(typed), typed >= 0
	case float64:
		converted := int64(typed)
		return converted, typed >= 0 && float64(converted) == typed
	default:
		return 0, false
	}
}

func safeArtifactAuthorityStringPayload(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok {
		return ""
	}
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(typed)
}

func safeArtifactAuthorityUUIDPayload(payload map[string]any, key string) (uuid.UUID, bool) {
	value, ok := payload[key]
	if !ok {
		return uuid.Nil, false
	}
	switch typed := value.(type) {
	case uuid.UUID:
		return typed, typed != uuid.Nil
	case string:
		parsed, err := uuid.Parse(strings.TrimSpace(typed))
		if err != nil || parsed == uuid.Nil {
			return uuid.Nil, false
		}
		return parsed, true
	default:
		return uuid.Nil, false
	}
}
