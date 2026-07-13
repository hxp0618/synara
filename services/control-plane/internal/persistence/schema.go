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
		&AgentSession{}, &AgentTurn{}, &SessionEvent{}, &Automation{}, &WorkerInstance{},
		&AgentExecution{}, &WorkerLease{}, &WorkerRequestReceipt{}, &APIIdempotencyKey{}, &ExecutionInteraction{},
		&ExecutionControlCommand{}, &Artifact{},
		&ArtifactPayloadMigration{}, &ArtifactAccessToken{}, &ProviderCredential{},
		&WorkerManifest{}, &WorkerProviderManifest{}, &ProviderRuntimeBinding{}, &RemoteWorkspace{},
		&WorkspaceMaterialization{}, &WorkspaceCleanupCommand{}, &WorkspaceCheckpoint{},
		&SSEConnectionLease{},
		&TenantRetentionPolicy{}, &IdentityConnection{}, &IdentityLoginAttempt{},
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
