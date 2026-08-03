package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6OperationsExercise retains the exact deployed browser-operations
// receipt referenced by one release candidate. Internal approval does not
// authenticate the deployment, browser sessions, Audit request IDs, evidence
// files, signatures, execution, or external approver authority.
type Stage6OperationsExercise struct {
	ID                                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                      uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID                     uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	Receipt                               []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256                         []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes                      int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                                string     `gorm:"column:receipt_schema;not null"`
	Assessment                            string     `gorm:"column:assessment;not null"`
	ReleaseCommit                         string     `gorm:"column:release_commit;not null"`
	EnvironmentClass                      string     `gorm:"column:environment_class;not null"`
	EnvironmentID                         string     `gorm:"column:environment_id;not null"`
	MatrixSHA256                          []byte     `gorm:"column:matrix_sha256;not null"`
	WebOrigin                             string     `gorm:"column:web_origin;not null"`
	AdminOrigin                           string     `gorm:"column:admin_origin;not null"`
	StartedAt                             time.Time  `gorm:"column:started_at;not null"`
	CompletedAt                           time.Time  `gorm:"column:completed_at;not null"`
	ValidatedAt                           time.Time  `gorm:"column:validated_at;not null"`
	AccountCount                          int64      `gorm:"column:account_count;not null"`
	OperationCount                        int64      `gorm:"column:operation_count;not null"`
	MatrixProfile                         string     `gorm:"column:matrix_profile;not null;default:internal-self-hosted-v3"`
	EvidenceFileCount                     int64      `gorm:"column:evidence_file_count;not null"`
	AllOperationsPassed                   bool       `gorm:"column:all_operations_passed;not null"`
	AllNegativeAuthorizationsDenied       bool       `gorm:"column:all_negative_authorizations_denied;not null"`
	NoDeveloperFallbacks                  bool       `gorm:"column:no_developer_fallbacks;not null"`
	ProductionAuthenticationDeclared      bool       `gorm:"column:production_authentication_declared;not null"`
	SupportLifecycleComplete              bool       `gorm:"column:support_lifecycle_complete;not null"`
	ReceiptApprovalsComplete              bool       `gorm:"column:receipt_approvals_complete;not null"`
	ReleaseEligibleEnvironment            bool       `gorm:"column:release_eligible_environment;not null"`
	EligibleForHumanGateReview            bool       `gorm:"column:eligible_for_human_gate_review;not null"`
	CryptographicSignaturesVerified       bool       `gorm:"column:cryptographic_signatures_verified;not null"`
	ExternalAuthorityVerificationRequired bool       `gorm:"column:external_authority_verification_required;not null"`
	State                                 string     `gorm:"column:state;not null"`
	Version                               int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                             uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	ApprovedAt                            *time.Time `gorm:"column:approved_at"`
	RejectedAt                            *time.Time `gorm:"column:rejected_at"`
	CreatedAt                             time.Time  `gorm:"column:created_at"`
	UpdatedAt                             time.Time  `gorm:"column:updated_at"`
}

func (Stage6OperationsExercise) TableName() string { return "stage6_operations_exercises" }

type Stage6OperationsExerciseApproval struct {
	ID                   uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperationsExerciseID uuid.UUID  `gorm:"column:operations_exercise_id;type:uuid;not null"`
	OperatorTenantID     uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role                 string     `gorm:"column:approval_role;not null"`
	Decision             string     `gorm:"column:decision;not null"`
	ApproverUserID       uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason               string     `gorm:"column:reason;not null"`
	EvidenceReference    string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256       *string    `gorm:"column:evidence_sha256"`
	SupersededAt         *time.Time `gorm:"column:superseded_at"`
	SupersededReason     *string    `gorm:"column:superseded_reason"`
	CreatedAt            time.Time  `gorm:"column:created_at"`
}

func (Stage6OperationsExerciseApproval) TableName() string {
	return "stage6_operations_exercise_approvals"
}
