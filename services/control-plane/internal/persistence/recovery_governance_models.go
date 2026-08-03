package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6RecoveryDrill retains an exact validator receipt and the candidate it
// was derived from. Its approved state is an internal governance decision, not
// proof that an external backup authority or signature is authentic.
type Stage6RecoveryDrill struct {
	ID                                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                      uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID                     uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	DrillID                               uuid.UUID  `gorm:"column:drill_id;type:uuid;not null;uniqueIndex"`
	Receipt                               []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256                         []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes                      int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                                string     `gorm:"column:receipt_schema;not null"`
	Assessment                            string     `gorm:"column:assessment;not null"`
	CandidateBindingSHA256                []byte     `gorm:"column:candidate_binding_sha256;not null"`
	RecoverySubjectSHA256                 []byte     `gorm:"column:recovery_subject_sha256;not null"`
	StartedAt                             time.Time  `gorm:"column:started_at;not null"`
	CompletedAt                           time.Time  `gorm:"column:completed_at;not null"`
	ValidatedAt                           time.Time  `gorm:"column:validated_at;not null"`
	MeasurementsWithinObjectives          bool       `gorm:"column:measurements_within_objectives;not null"`
	AllRestoreCanariesPassed              bool       `gorm:"column:all_restore_canaries_passed;not null"`
	AllSourceApprovalsApproved            bool       `gorm:"column:all_source_approvals_approved;not null"`
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

func (Stage6RecoveryDrill) TableName() string { return "stage6_recovery_drills" }

type Stage6RecoveryComponent struct {
	ID                    uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	RecoveryDrillRecordID uuid.UUID `gorm:"column:recovery_drill_record_id;type:uuid;not null"`
	OperatorTenantID      uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ComponentKey          string    `gorm:"column:component_key;not null"`
	Profile               string    `gorm:"column:profile;not null"`
	SourceRegion          string    `gorm:"column:source_region;not null"`
	RestoreRegion         string    `gorm:"column:restore_region;not null"`
	MeasuredRPOSeconds    float64   `gorm:"column:measured_rpo_seconds;not null"`
	RPOObjectiveSeconds   float64   `gorm:"column:rpo_objective_seconds;not null"`
	MeasuredRTOSeconds    float64   `gorm:"column:measured_rto_seconds;not null"`
	RTOObjectiveSeconds   float64   `gorm:"column:rto_objective_seconds;not null"`
	RPOWithinObjective    bool      `gorm:"column:rpo_within_objective;not null"`
	RTOWithinObjective    bool      `gorm:"column:rto_within_objective;not null"`
	RestoreServedCanary   bool      `gorm:"column:restore_served_canary;not null"`
	CreatedAt             time.Time `gorm:"column:created_at"`
}

func (Stage6RecoveryComponent) TableName() string { return "stage6_recovery_components" }

type Stage6RecoveryApproval struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	RecoveryDrillRecordID uuid.UUID  `gorm:"column:recovery_drill_record_id;type:uuid;not null"`
	OperatorTenantID      uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role                  string     `gorm:"column:approval_role;not null"`
	Decision              string     `gorm:"column:decision;not null"`
	ApproverUserID        uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason                string     `gorm:"column:reason;not null"`
	EvidenceReference     string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256        *string    `gorm:"column:evidence_sha256"`
	SupersededAt          *time.Time `gorm:"column:superseded_at"`
	SupersededReason      *string    `gorm:"column:superseded_reason"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
}

func (Stage6RecoveryApproval) TableName() string { return "stage6_recovery_approvals" }
