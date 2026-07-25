package persistence

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func AllModels() []any {
	return []any{
		&PlatformInstallation{}, &MetadataImport{}, &User{}, &UserIdentity{}, &Tenant{},
		&TenantMembership{}, &Organization{}, &OrganizationMembership{}, &LoginSession{},
		&TenantInvitation{}, &AuditLog{}, &OutboxMessage{}, &TenantQuota{}, &Project{}, &ExecutionTarget{},
		&AgentSession{}, &AgentTurn{}, &SessionEvent{}, &Automation{}, &WorkerInstance{}, &WorkerIdentityTombstone{},
		&AgentExecution{}, &ExecutionRecoveryBundle{}, &ExecutionSuspendAttempt{}, &WorkerLease{}, &WorkerRequestReceipt{}, &APIIdempotencyKey{}, &ExecutionInteraction{},
		&ExecutionControlCommand{}, &Artifact{},
		&ArtifactPayloadMigration{}, &ArtifactAccessToken{}, &ProviderCredential{}, &ProviderCredentialScopePolicy{},
		&AgentMemoryHead{}, &AgentMemoryRevision{},
		&CredentialBinding{}, &ExecutionCredentialGrant{}, &ExecutionProviderCredentialGrant{},
		&WorkerManifest{}, &WorkerProviderManifest{}, &WorkerReleaseRevision{}, &WorkerReleasePolicy{},
		&WorkerReleaseTransition{}, &WorkerReleaseAutoRollbackWindow{}, &WorkerPool{}, &ExecutionPlacementPolicy{},
		&WorkerPoolWarmCapacity{},
		&ExecutionTargetGroup{}, &ExecutionTargetGroupMember{}, &ExecutionTargetHealth{},
		&ExecutionTargetDRReadiness{}, &ExecutionLocationOutage{}, &ExecutionFailoverAttempt{},
		&ExecutionGenerationFact{}, &WorkerIncarnationFact{}, &WorkerClaimFact{},
		&BillingProviderTariff{}, &BillingEstimatedUsageCharge{}, &BillingActualInvoiceImport{}, &BillingActualInvoiceLine{},
		&ProviderRuntimeBinding{}, &RemoteWorkspace{},
		&WorkspaceMaterialization{}, &WorkspaceCleanupCommand{}, &WorkspaceCheckpoint{},
		&SSEConnectionLease{}, &ReconcilerLease{},
		&TenantRetentionPolicy{}, &TenantResourceLifecyclePolicy{}, &ProjectResourceLifecyclePolicy{},
		&IdentityConnection{}, &IdentityLoginAttempt{},
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
