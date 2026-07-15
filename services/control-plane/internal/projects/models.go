package projects

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Project struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenantId"`
	OrganizationID  uuid.UUID  `json:"organizationId"`
	Name            string     `json:"name"`
	RepositoryURL   *string    `json:"repositoryUrl"`
	DefaultBranch   string     `json:"defaultBranch"`
	GitCredentialID *uuid.UUID `json:"gitCredentialId"`
	Visibility      string     `json:"visibility"`
	CreatedBy       uuid.UUID  `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	ArchivedAt      *time.Time `json:"archivedAt"`
}

type CreateProjectInput struct {
	Name            string               `json:"name"`
	RepositoryURL   *string              `json:"repositoryUrl"`
	DefaultBranch   string               `json:"defaultBranch"`
	GitCredentialID OptionalNullableUUID `json:"gitCredentialId"`
	Visibility      string               `json:"visibility"`
}

type UpdateProjectInput struct {
	Name            *string              `json:"name"`
	RepositoryURL   *string              `json:"repositoryUrl"`
	DefaultBranch   *string              `json:"defaultBranch"`
	GitCredentialID OptionalNullableUUID `json:"gitCredentialId"`
	Visibility      *string              `json:"visibility"`
}

type OptionalNullableUUID struct {
	Set   bool
	Value *uuid.UUID
}

func (value *OptionalNullableUUID) UnmarshalJSON(payload []byte) error {
	value.Set = true
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		value.Value = nil
		return nil
	}
	var parsed uuid.UUID
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return err
	}
	value.Value = &parsed
	return nil
}
