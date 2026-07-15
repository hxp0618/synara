package persistence

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func TestModelsHaveValidGormSchemas(t *testing.T) {
	models := []any{
		&User{}, &LoginSession{}, &Tenant{}, &TenantMembership{}, &Organization{},
		&OrganizationMembership{}, &TenantInvitation{}, &AuditLog{}, &Project{},
		&AgentSession{}, &AgentTurn{}, &SessionEvent{}, &Automation{}, &WorkerInstance{}, &WorkerIdentityTombstone{},
		&AgentExecution{}, &WorkerLease{}, &WorkerRequestReceipt{}, &APIIdempotencyKey{}, &ExecutionInteraction{}, &OutboxMessage{},
	}
	for _, model := range models {
		if _, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{}); err != nil {
			t.Fatalf("parse %T: %v", model, err)
		}
	}
}
