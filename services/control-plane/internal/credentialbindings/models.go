package credentialbindings

import (
	"time"

	"github.com/google/uuid"
)

type Binding struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenantId"`
	OrganizationID    *uuid.UUID `json:"organizationId"`
	ProjectID         *uuid.UUID `json:"projectId"`
	ExecutionTargetID *uuid.UUID `json:"executionTargetId"`
	CredentialID      uuid.UUID  `json:"credentialId"`
	BindingKind       string     `json:"bindingKind"`
	SelectorValue     string     `json:"selector"`
	CreatedBy         uuid.UUID  `json:"createdBy"`
	CreatedAt         time.Time  `json:"createdAt"`
	DisabledAt        *time.Time `json:"disabledAt"`
	DisabledBy        *uuid.UUID `json:"disabledBy"`
}

type CreateInput struct {
	ProjectID         *uuid.UUID `json:"projectId"`
	ExecutionTargetID *uuid.UUID `json:"executionTargetId"`
	CredentialID      uuid.UUID  `json:"credentialId"`
	BindingKind       string     `json:"bindingKind"`
	SelectorValue     *string    `json:"selector,omitempty"`
}

type OwnerFilter struct {
	ProjectID         *uuid.UUID
	ExecutionTargetID *uuid.UUID
}
