package projects

import (
	"time"

	"github.com/google/uuid"
)

type Project struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenantId"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	Name           string     `json:"name"`
	RepositoryURL  *string    `json:"repositoryUrl"`
	DefaultBranch  string     `json:"defaultBranch"`
	Visibility     string     `json:"visibility"`
	CreatedBy      uuid.UUID  `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ArchivedAt     *time.Time `json:"archivedAt"`
}

type CreateProjectInput struct {
	Name          string  `json:"name"`
	RepositoryURL *string `json:"repositoryUrl"`
	DefaultBranch string  `json:"defaultBranch"`
	Visibility    string  `json:"visibility"`
}

type UpdateProjectInput struct {
	Name          *string `json:"name"`
	RepositoryURL *string `json:"repositoryUrl"`
	DefaultBranch *string `json:"defaultBranch"`
	Visibility    *string `json:"visibility"`
}
