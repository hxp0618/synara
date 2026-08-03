package persistence

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func AllModels() []any {
	return []any{
		&PlatformInstallation{}, &MetadataImport{}, &User{}, &UserIdentity{}, &Tenant{},
		&TenantMembership{}, &Organization{}, &OrganizationMembership{}, &DesktopDevice{}, &LoginSession{}, &DesktopEnrollment{},
		&SaaSPlan{}, &PlanEntitlement{}, &TenantSubscription{}, &FeatureFlag{}, &TenantFeatureFlagOverride{},
		&CommercialBillingCheckoutSession{}, &CommercialBillingProviderEvent{},
		&Stage6ReleaseCandidate{}, &Stage6ReleaseProviderAuthorizationBinding{}, &Stage6ReleaseApproval{}, &Stage6ReleaseFinalReview{},
		&Stage6Incident{}, &Stage6IncidentUpdate{}, &Stage6IncidentResolutionApproval{},
		&Stage6IncidentExercise{}, &Stage6IncidentExerciseApproval{},
		&Stage6OperationsExercise{}, &Stage6OperationsExerciseApproval{},
		&Stage6BillingExercise{}, &Stage6BillingExerciseApproval{},
		&Stage6InternalCostReview{}, &Stage6InternalCostReviewApproval{},
		&Stage6SLOWindow{}, &Stage6SLOObjective{}, &Stage6SLOApproval{},
		&Stage6RecoveryDrill{}, &Stage6RecoveryComponent{}, &Stage6RecoveryApproval{},
		&Stage6PenetrationEngagement{}, &Stage6PenetrationAsset{}, &Stage6PenetrationApproval{},
		&Stage6CapacityRun{}, &Stage6CapacityPhase{}, &Stage6CapacityApproval{},
		&Stage6ComplianceProgram{}, &Stage6ComplianceControl{}, &Stage6ComplianceEvidence{},
		&Stage6ComplianceEvidenceReview{}, &Stage6ComplianceProgramDecision{},
		&ProviderCommercialAuthorization{}, &ProviderCommercialAuthorizationApproval{},
		&Stage6GovernanceAuthorityGrant{},
		&TenantSupportPolicy{}, &SupportAccessGrant{}, &LegalHold{}, &PrivacyRequest{}, &PrivacyRequestEvent{}, &TenantDataExport{},
		&TenantInvitation{}, &AuditLog{}, &OutboxMessage{}, &OutboxPressureState{}, &TenantQuota{}, &ExecutionQuotaPolicy{}, &Project{}, &ProjectCostAllocation{}, &ExecutionTarget{}, &KubernetesPodDeletionFence{},
		&AgentSession{}, &AgentTurn{}, &SessionEvent{}, &Automation{}, &WorkerInstance{}, &WorkerStorageScrub{}, &WorkerIdentityTombstone{},
		&ExecutionUsageSummary{}, &TenantUsageQuotaAlert{},
		&AgentExecution{}, &ExecutionSchedulingDecision{}, &ExecutionSchedulingCandidate{}, &ExecutionCapacityAdmission{}, &ExecutionRecoveryBundle{}, &ExecutionSuspendAttempt{}, &WorkerLease{}, &WorkerRequestReceipt{}, &APIIdempotencyKey{}, &ExecutionInteraction{},
		&ExecutionControlCommand{}, &Artifact{},
		&ArtifactPayloadMigration{}, &ArtifactAccessToken{}, &ProviderCredential{}, &ProviderCredentialScopePolicy{},
		&KMSRewrapRun{}, &KMSRewrapEntry{}, &KMSRewrapReceipt{},
		&RuntimeSecretRekeyRun{}, &RuntimeSecretRekeyEntry{}, &RuntimeSecretRekeyReceipt{},
		&AgentMemoryHead{}, &AgentMemoryRevision{},
		&CredentialBinding{}, &ExecutionCredentialGrant{}, &ExecutionProviderCredentialGrant{},
		&WorkerManifest{}, &WorkerProviderManifest{}, &WorkerReleaseRevision{}, &WorkerReleasePolicy{},
		&WorkerReleaseTransition{}, &WorkerReleaseAutoRollbackWindow{}, &WorkerPool{}, &ExecutionPlacementPolicy{},
		&WorkerPoolWarmCapacity{}, &WorkerPoolAutoscalingPolicy{}, &WorkerPoolAutoscalingState{},
		&ExecutionTargetCapacity{}, &ExecutionTargetGroup{}, &ExecutionTargetGroupMember{}, &ExecutionTargetHealth{}, &ExecutionTargetReservationAcknowledgement{},
		&ExecutionTargetDRReadiness{}, &ExecutionLocationOutage{}, &ExecutionFailoverAttempt{},
		&PlatformRoutingPublication{},
		&ExecutionSchedulingPolicyHead{}, &ExecutionSchedulingPolicyRevision{},
		&ExecutionSchedulingPolicyRule{}, &ExecutionSchedulingPolicyRuleValue{},
		&ExecutionGenerationFact{}, &ExecutionGenerationPodFailureFact{}, &ExecutionKubernetesAllocation{},
		&ExecutionRuntimeIsolationDecision{},
		&ExecutionTargetRuntimeIsolationObservation{},
		&WorkerIncarnationFact{}, &WorkerIncarnationMetricRollup{}, &WorkerIncarnationMetricRollupEntry{},
		&ExecutionGenerationMetricRollup{}, &ExecutionGenerationMetricRollupEntry{},
		&ExecutionGenerationPodFailureMetricRollupEntry{},
		&WorkerClaimFact{}, &WorkerClaimReleaseFact{},
		&BillingProviderTariff{}, &BillingEstimatedUsageCharge{}, &BillingActualInvoiceImport{}, &BillingActualInvoiceLine{},
		&BillingSharedTargetLedgerCoverage{}, &BillingSharedCostAllocationRun{}, &BillingSharedEstimatedChargeSlice{},
		&BillingSharedAllocationSchedulePeriod{},
		&BillingSharedActualAllocationRun{}, &BillingSharedActualAllocationLine{}, &BillingSharedActualChargeSlice{},
		&ProviderRuntimeBinding{}, &RemoteWorkspace{},
		&WorkspaceMaterialization{}, &WorkspaceCleanupCommand{}, &WorkspaceCheckpoint{},
		&SSEConnectionLease{}, &ReconcilerLease{},
		&TenantRetentionPolicy{}, &TenantResourceLifecyclePolicy{}, &ProjectResourceLifecyclePolicy{},
		&IdentityConnection{}, &TenantDomain{}, &TenantIdentityPolicy{}, &IdentityLoginAttempt{},
		&ServiceAccount{}, &ServiceAccountToken{}, &IdentityGroup{}, &IdentityGroupMember{},
		&IdentityGroupMapping{},
	}
}

func WithLocking(db *gorm.DB, strength, options string) *gorm.DB {
	if db.Dialector.Name() != "postgres" {
		return db
	}
	return db.Clauses(clause.Locking{Strength: strength, Options: options})
}
