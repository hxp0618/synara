package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6IncidentExercise retains the exact paging and internal-communication
// exercise receipt referenced by one release candidate. Internal approval does
// not authenticate the deployed Status Board, paging provider, employee
// notification delivery, evidence signatures, execution, or approver authority.
type Stage6IncidentExercise struct {
	ID                                      uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                        uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID                       uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	ExerciseID                              uuid.UUID  `gorm:"column:exercise_id;type:uuid;not null;uniqueIndex"`
	Receipt                                 []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256                           []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes                        int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                                  string     `gorm:"column:receipt_schema;not null"`
	Assessment                              string     `gorm:"column:assessment;not null"`
	ReleaseCommit                           string     `gorm:"column:release_commit;not null"`
	EnvironmentClass                        string     `gorm:"column:environment_class;not null"`
	EnvironmentID                           string     `gorm:"column:environment_id;not null"`
	ExerciseMode                            string     `gorm:"column:exercise_mode;not null"`
	Severity                                string     `gorm:"column:severity;not null"`
	ServiceOrigin                           string     `gorm:"column:service_origin;not null"`
	InternalStatusBoardOrigin               string     `gorm:"column:status_page_origin;not null"`
	StartedAt                               time.Time  `gorm:"column:started_at;not null"`
	CompletedAt                             time.Time  `gorm:"column:completed_at;not null"`
	ValidatedAt                             time.Time  `gorm:"column:validated_at;not null"`
	IndependentInternalStatusBoardDeclared  bool       `gorm:"column:independent_status_page_declared;not null"`
	RoleSeparationComplete                  bool       `gorm:"column:role_separation_complete;not null"`
	PagingExerciseComplete                  bool       `gorm:"column:paging_exercise_complete;not null"`
	InternalStatusBoardComponentsComplete   bool       `gorm:"column:status_page_components_complete;not null"`
	InternalTimelineWithinTargets           bool       `gorm:"column:public_timeline_within_targets;not null"`
	EmployeeNotificationDeliveryComplete    bool       `gorm:"column:subscriber_delivery_complete;not null"`
	RecoveryVerificationComplete            bool       `gorm:"column:recovery_verification_complete;not null"`
	ReviewComplete                          bool       `gorm:"column:review_complete;not null"`
	ReleaseEligibleEnvironment              bool       `gorm:"column:release_eligible_environment;not null"`
	EligibleForHumanGateReview              bool       `gorm:"column:eligible_for_human_gate_review;not null"`
	CryptographicSignaturesVerified         bool       `gorm:"column:cryptographic_signatures_verified;not null"`
	DeploymentAuthorityVerificationRequired bool       `gorm:"column:external_authority_verification_required;not null"`
	State                                   string     `gorm:"column:state;not null"`
	Version                                 int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                               uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	ApprovedAt                              *time.Time `gorm:"column:approved_at"`
	RejectedAt                              *time.Time `gorm:"column:rejected_at"`
	CreatedAt                               time.Time  `gorm:"column:created_at"`
	UpdatedAt                               time.Time  `gorm:"column:updated_at"`
}

func (Stage6IncidentExercise) TableName() string { return "stage6_incident_exercises" }

type Stage6IncidentExerciseApproval struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	IncidentExerciseID uuid.UUID  `gorm:"column:incident_exercise_id;type:uuid;not null"`
	OperatorTenantID   uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role               string     `gorm:"column:approval_role;not null"`
	Decision           string     `gorm:"column:decision;not null"`
	ApproverUserID     uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason             string     `gorm:"column:reason;not null"`
	EvidenceReference  string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256     *string    `gorm:"column:evidence_sha256"`
	SupersededAt       *time.Time `gorm:"column:superseded_at"`
	SupersededReason   *string    `gorm:"column:superseded_reason"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
}

func (Stage6IncidentExerciseApproval) TableName() string {
	return "stage6_incident_exercise_approvals"
}
