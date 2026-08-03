package persistence

import (
	"time"

	"github.com/google/uuid"
)

type Stage6ComplianceProgram struct {
	ID                            uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID              uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ProgramKey                    string     `gorm:"column:program_key;not null;uniqueIndex"`
	Framework                     string     `gorm:"column:framework;not null"`
	ScopeVersion                  string     `gorm:"column:scope_version;not null"`
	ScopeSummary                  string     `gorm:"column:scope_summary;not null"`
	ExecutiveSponsorUserID        uuid.UUID  `gorm:"column:executive_sponsor_user_id;type:uuid;not null"`
	AuditorOrganization           string     `gorm:"column:auditor_organization;not null"`
	AuditorEngagementReference    string     `gorm:"column:auditor_engagement_reference;not null"`
	ObservationStart              time.Time  `gorm:"column:observation_start;not null"`
	ObservationEnd                time.Time  `gorm:"column:observation_end;not null"`
	EvidenceRepositoryReference   string     `gorm:"column:evidence_repository_reference;not null"`
	EvidenceAccessPolicyReference string     `gorm:"column:evidence_access_policy_reference;not null"`
	EvidenceRetentionDays         int        `gorm:"column:evidence_retention_days;not null"`
	VendorRegisterReference       string     `gorm:"column:vendor_register_reference;not null"`
	RiskRegisterReference         string     `gorm:"column:risk_register_reference;not null"`
	State                         string     `gorm:"column:state;not null"`
	Version                       int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                     uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	RecordCompletedAt             *time.Time `gorm:"column:record_completed_at"`
	CreatedAt                     time.Time  `gorm:"column:created_at"`
	UpdatedAt                     time.Time  `gorm:"column:updated_at"`
}

func (Stage6ComplianceProgram) TableName() string { return "stage6_compliance_programs" }

type Stage6ComplianceControl struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ProgramID           uuid.UUID `gorm:"column:program_id;type:uuid;not null"`
	OperatorTenantID    uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ControlID           string    `gorm:"column:control_id;not null"`
	Family              string    `gorm:"column:control_family;not null"`
	Title               string    `gorm:"column:title;not null"`
	Description         string    `gorm:"column:description;not null"`
	OwnerUserID         uuid.UUID `gorm:"column:owner_user_id;type:uuid;not null"`
	Cadence             string    `gorm:"column:cadence;not null"`
	EvidenceRequirement string    `gorm:"column:evidence_requirement;not null"`
	CreatedBy           uuid.UUID `gorm:"column:created_by;type:uuid;not null"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (Stage6ComplianceControl) TableName() string { return "stage6_compliance_controls" }

type Stage6ComplianceEvidence struct {
	ID               uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ProgramID        uuid.UUID `gorm:"column:program_id;type:uuid;not null"`
	ControlRecordID  uuid.UUID `gorm:"column:control_record_id;type:uuid;not null"`
	OperatorTenantID uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	EvidenceID       string    `gorm:"column:evidence_id;not null"`
	EvidenceType     string    `gorm:"column:evidence_type;not null"`
	PeriodStart      time.Time `gorm:"column:period_start;not null"`
	PeriodEnd        time.Time `gorm:"column:period_end;not null"`
	SourceReference  string    `gorm:"column:source_reference;not null"`
	SHA256           []byte    `gorm:"column:sha256;not null"`
	MediaType        string    `gorm:"column:media_type;not null"`
	Classification   string    `gorm:"column:classification;not null"`
	CollectedAt      time.Time `gorm:"column:collected_at;not null"`
	RetentionUntil   time.Time `gorm:"column:retention_until;not null"`
	SubmittedBy      uuid.UUID `gorm:"column:submitted_by;type:uuid;not null"`
	CreatedAt        time.Time `gorm:"column:created_at"`
}

func (Stage6ComplianceEvidence) TableName() string { return "stage6_compliance_evidence" }

type Stage6ComplianceEvidenceReview struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	EvidenceRecordID  uuid.UUID  `gorm:"column:evidence_record_id;type:uuid;not null"`
	ProgramID         uuid.UUID  `gorm:"column:program_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Decision          string     `gorm:"column:decision;not null"`
	ReviewRole        string     `gorm:"column:review_role;not null"`
	ReviewerUserID    uuid.UUID  `gorm:"column:reviewer_user_id;type:uuid;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string    `gorm:"column:evidence_sha256"`
	SupersededAt      *time.Time `gorm:"column:superseded_at"`
	SupersededReason  *string    `gorm:"column:superseded_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

func (Stage6ComplianceEvidenceReview) TableName() string {
	return "stage6_compliance_evidence_reviews"
}

type Stage6ComplianceProgramDecision struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ProgramID         uuid.UUID  `gorm:"column:program_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	DecisionRole      string     `gorm:"column:decision_role;not null"`
	Decision          string     `gorm:"column:decision;not null"`
	DeciderUserID     uuid.UUID  `gorm:"column:decider_user_id;type:uuid;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string    `gorm:"column:evidence_sha256"`
	SupersededAt      *time.Time `gorm:"column:superseded_at"`
	SupersededReason  *string    `gorm:"column:superseded_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

func (Stage6ComplianceProgramDecision) TableName() string {
	return "stage6_compliance_program_decisions"
}
