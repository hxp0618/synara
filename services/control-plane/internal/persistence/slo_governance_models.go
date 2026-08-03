package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6SLOWindow retains the exact validated SLO receipt and its release
// identity. It is an internal review authority, not a production SLO claim.
type Stage6SLOWindow struct {
	ID                         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID           uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID          uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	WindowID                   uuid.UUID  `gorm:"column:window_id;type:uuid;not null;uniqueIndex"`
	Receipt                    []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256              []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes           int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                     string     `gorm:"column:receipt_schema;not null"`
	Assessment                 string     `gorm:"column:assessment;not null"`
	ReleaseCommit              string     `gorm:"column:release_commit;not null"`
	EnvironmentClass           string     `gorm:"column:environment_class;not null"`
	EnvironmentID              string     `gorm:"column:environment_id;not null"`
	PublicOrigin               string     `gorm:"column:public_origin;not null"`
	WindowStartedAt            time.Time  `gorm:"column:window_started_at;not null"`
	WindowCompletedAt          time.Time  `gorm:"column:window_completed_at;not null"`
	ValidatedAt                time.Time  `gorm:"column:validated_at;not null"`
	QueryRevision              string     `gorm:"column:query_revision;not null"`
	AllObjectivesAssessable    bool       `gorm:"column:all_objectives_assessable;not null"`
	AllObjectivesMet           bool       `gorm:"column:all_objectives_met;not null"`
	EligibleForHumanGateReview bool       `gorm:"column:eligible_for_human_gate_review;not null"`
	WorstBudgetRemainingRatio  float64    `gorm:"column:worst_budget_remaining_ratio;not null"`
	BudgetPolicyState          string     `gorm:"column:budget_policy_state;not null"`
	State                      string     `gorm:"column:state;not null"`
	Version                    int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                  uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	ApprovedAt                 *time.Time `gorm:"column:approved_at"`
	RejectedAt                 *time.Time `gorm:"column:rejected_at"`
	CreatedAt                  time.Time  `gorm:"column:created_at"`
	UpdatedAt                  time.Time  `gorm:"column:updated_at"`
}

func (Stage6SLOWindow) TableName() string { return "stage6_slo_windows" }

type Stage6SLOObjective struct {
	ID                        uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	SLOWindowRecordID         uuid.UUID `gorm:"column:slo_window_record_id;type:uuid;not null"`
	OperatorTenantID          uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ObjectiveKey              string    `gorm:"column:objective_key;not null"`
	TargetRatio               float64   `gorm:"column:target_ratio;not null"`
	GoodRatio                 float64   `gorm:"column:good_ratio;not null"`
	SampleCount               int64     `gorm:"column:sample_count;not null"`
	ErrorBudgetRemainingRatio float64   `gorm:"column:error_budget_remaining_ratio;not null"`
	PolicyState               string    `gorm:"column:policy_state;not null"`
	Assessable                bool      `gorm:"column:assessable;not null"`
	ObjectiveMet              bool      `gorm:"column:objective_met;not null"`
	CreatedAt                 time.Time `gorm:"column:created_at"`
}

func (Stage6SLOObjective) TableName() string { return "stage6_slo_objectives" }

type Stage6SLOApproval struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	SLOWindowRecordID uuid.UUID  `gorm:"column:slo_window_record_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role              string     `gorm:"column:approval_role;not null"`
	Decision          string     `gorm:"column:decision;not null"`
	ApproverUserID    uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string    `gorm:"column:evidence_sha256"`
	SupersededAt      *time.Time `gorm:"column:superseded_at"`
	SupersededReason  *string    `gorm:"column:superseded_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

func (Stage6SLOApproval) TableName() string { return "stage6_slo_approvals" }
