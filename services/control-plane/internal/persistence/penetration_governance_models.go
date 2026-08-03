package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6PenetrationEngagement retains an exact validator receipt bound to one
// release candidate. Approval is an internal governance decision; it does not
// authenticate the external assessor, signed report, or execution evidence.
type Stage6PenetrationEngagement struct {
	ID                                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                      uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID                     uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	EngagementID                          uuid.UUID  `gorm:"column:engagement_id;type:uuid;not null;uniqueIndex"`
	Receipt                               []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256                         []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes                      int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                                string     `gorm:"column:receipt_schema;not null"`
	Assessment                            string     `gorm:"column:assessment;not null"`
	ReleaseCommit                         string     `gorm:"column:release_commit;not null"`
	EnvironmentClass                      string     `gorm:"column:environment_class;not null"`
	EnvironmentID                         string     `gorm:"column:environment_id;not null"`
	DeploymentProfile                     string     `gorm:"column:deployment_profile;not null"`
	StartedAt                             time.Time  `gorm:"column:started_at;not null"`
	CompletedAt                           time.Time  `gorm:"column:completed_at;not null"`
	ReportIssuedAt                        time.Time  `gorm:"column:report_issued_at;not null"`
	ValidatedAt                           time.Time  `gorm:"column:validated_at;not null"`
	ThirdPartyIndependenceDeclared        bool       `gorm:"column:third_party_independence_declared;not null"`
	Stage5DependencySatisfied             bool       `gorm:"column:stage5_dependency_satisfied;not null"`
	AssetCoverageComplete                 bool       `gorm:"column:asset_coverage_complete;not null"`
	ScopeCoverageComplete                 bool       `gorm:"column:scope_coverage_complete;not null"`
	MethodologyCoverageComplete           bool       `gorm:"column:methodology_coverage_complete;not null"`
	NoUnacceptedHighOrCriticalFindings    bool       `gorm:"column:no_unaccepted_high_or_critical_findings;not null"`
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

func (Stage6PenetrationEngagement) TableName() string { return "stage6_penetration_engagements" }

type Stage6PenetrationAsset struct {
	ID                      uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	PenetrationEngagementID uuid.UUID `gorm:"column:penetration_engagement_id;type:uuid;not null"`
	OperatorTenantID        uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	AssetType               string    `gorm:"column:asset_type;not null"`
	ArtifactSHA256          []byte    `gorm:"column:artifact_sha256;not null"`
	Tested                  bool      `gorm:"column:tested;not null"`
	CreatedAt               time.Time `gorm:"column:created_at"`
}

func (Stage6PenetrationAsset) TableName() string { return "stage6_penetration_assets" }

type Stage6PenetrationApproval struct {
	ID                      uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	PenetrationEngagementID uuid.UUID  `gorm:"column:penetration_engagement_id;type:uuid;not null"`
	OperatorTenantID        uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role                    string     `gorm:"column:approval_role;not null"`
	Decision                string     `gorm:"column:decision;not null"`
	ApproverUserID          uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason                  string     `gorm:"column:reason;not null"`
	EvidenceReference       string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256          *string    `gorm:"column:evidence_sha256"`
	SupersededAt            *time.Time `gorm:"column:superseded_at"`
	SupersededReason        *string    `gorm:"column:superseded_reason"`
	CreatedAt               time.Time  `gorm:"column:created_at"`
}

func (Stage6PenetrationApproval) TableName() string { return "stage6_penetration_approvals" }
