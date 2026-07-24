package workerreleases

import (
	"time"

	"github.com/google/uuid"
)

const (
	ChannelPromoted = "promoted"
	ChannelCanary   = "canary"

	DefaultAutoRollbackObservationWindow  = 15 * time.Minute
	DefaultAutoRollbackMinimumExecutions  = 4
	DefaultAutoRollbackFailureThreshold   = 2
	DefaultAutoRollbackFailureRatePercent = 50
)

type Revision struct {
	ID                 uuid.UUID `json:"id"`
	TenantID           uuid.UUID `json:"tenantId"`
	ExecutionTargetID  uuid.UUID `json:"executionTargetId"`
	Revision           int64     `json:"revision"`
	WorkerManifestID   uuid.UUID `json:"workerManifestId"`
	WorkerBuildVersion string    `json:"workerBuildVersion"`
	WorkerBuildGitSHA  *string   `json:"workerBuildGitSha,omitempty"`
	ImageDigest        *string   `json:"imageDigest,omitempty"`
	Description        string    `json:"description"`
	CreatedBy          uuid.UUID `json:"createdBy"`
	CreatedAt          time.Time `json:"createdAt"`
}

type Policy struct {
	TenantID           uuid.UUID  `json:"tenantId"`
	ExecutionTargetID  uuid.UUID  `json:"executionTargetId"`
	PolicyVersion      int64      `json:"policyVersion"`
	PromotedRevisionID uuid.UUID  `json:"promotedRevisionId"`
	CanaryRevisionID   *uuid.UUID `json:"canaryRevisionId,omitempty"`
	CanaryPercent      int        `json:"canaryPercent"`
	UpdatedBy          uuid.UUID  `json:"updatedBy"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

type Transition struct {
	ID                     uuid.UUID  `json:"id"`
	TenantID               uuid.UUID  `json:"tenantId"`
	ExecutionTargetID      uuid.UUID  `json:"executionTargetId"`
	PolicyVersion          int64      `json:"policyVersion"`
	Action                 string     `json:"action"`
	FromPromotedRevisionID *uuid.UUID `json:"fromPromotedRevisionId,omitempty"`
	FromCanaryRevisionID   *uuid.UUID `json:"fromCanaryRevisionId,omitempty"`
	ToPromotedRevisionID   uuid.UUID  `json:"toPromotedRevisionId"`
	ToCanaryRevisionID     *uuid.UUID `json:"toCanaryRevisionId,omitempty"`
	CanaryPercent          int        `json:"canaryPercent"`
	Reason                 string     `json:"reason"`
	ActorID                uuid.UUID  `json:"actorId"`
	RequestID              *string    `json:"requestId,omitempty"`
	OccurredAt             time.Time  `json:"occurredAt"`
}

type Overview struct {
	Policy       *Policy             `json:"policy"`
	AutoRollback *AutoRollbackWindow `json:"autoRollback,omitempty"`
	Revisions    []Revision          `json:"revisions"`
	Transitions  []Transition        `json:"transitions"`
}

type CreateRevisionInput struct {
	WorkerManifestID uuid.UUID `json:"workerManifestId"`
	Description      string    `json:"description"`
}

type PolicyChangeInput struct {
	ExpectedPolicyVersion int64              `json:"expectedPolicyVersion"`
	Reason                string             `json:"reason"`
	CanaryPercent         int                `json:"canaryPercent,omitempty"`
	AutoRollback          *AutoRollbackInput `json:"autoRollback,omitempty"`
}

type AutoRollbackInput struct {
	Enabled                  bool `json:"enabled"`
	ObservationWindowSeconds int  `json:"observationWindowSeconds,omitempty"`
	MinimumExecutions        int  `json:"minimumExecutions,omitempty"`
	FailureThreshold         int  `json:"failureThreshold,omitempty"`
	FailureRatePercent       int  `json:"failureRatePercent,omitempty"`
}

type AutoRollbackWindow struct {
	ID                  uuid.UUID      `json:"id"`
	PolicyVersion       int64          `json:"policyVersion"`
	CandidateRevisionID uuid.UUID      `json:"candidateRevisionId"`
	CandidateChannel    string         `json:"candidateChannel"`
	FallbackRevisionID  uuid.UUID      `json:"fallbackRevisionId"`
	StartedAt           time.Time      `json:"startedAt"`
	ExpiresAt           time.Time      `json:"expiresAt"`
	MinimumExecutions   int            `json:"minimumExecutions"`
	FailureThreshold    int            `json:"failureThreshold"`
	FailureRatePercent  int            `json:"failureRatePercent"`
	Status              string         `json:"status"`
	DecisionReason      *string        `json:"decisionReason,omitempty"`
	Evidence            map[string]any `json:"evidence"`
	DecisionAt          *time.Time     `json:"decisionAt,omitempty"`
	EnabledBy           uuid.UUID      `json:"enabledBy"`
}

type Selection struct {
	RevisionID uuid.UUID
	Channel    string
}

type OperationResult[T any] struct {
	Value      T
	Replayed   bool
	StatusCode int
}
