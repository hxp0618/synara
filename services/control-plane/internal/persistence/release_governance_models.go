package persistence

import (
	"time"

	"github.com/google/uuid"
)

type Stage6ReleaseCandidate struct {
	ID                             uuid.UUID        `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID               uuid.UUID        `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateID                    string           `gorm:"column:candidate_id;not null;uniqueIndex"`
	SourceCommit                   string           `gorm:"column:source_commit;not null"`
	LockfileSHA256                 []byte           `gorm:"column:lockfile_sha256;not null"`
	EvidenceBundleSHA256           []byte           `gorm:"column:evidence_bundle_sha256;not null"`
	EvidenceBundleReceipt          []byte           `gorm:"column:evidence_bundle_receipt"`
	EvidenceBundleSchema           string           `gorm:"column:evidence_bundle_schema"`
	EvidenceBundleAssessment       string           `gorm:"column:evidence_bundle_assessment"`
	EvidenceBundleValidatedAt      string           `gorm:"column:evidence_bundle_validated_at"`
	EvidenceBundleReceiptSizeBytes int64            `gorm:"column:evidence_bundle_receipt_size_bytes"`
	DesktopArtifactSetSHA256       string           `gorm:"column:desktop_artifact_set_sha256"`
	EvidenceReceiptBound           bool             `gorm:"column:evidence_receipt_bound;not null;default:false"`
	FinalAssetSetSHA256            []byte           `gorm:"column:final_asset_set_sha256;not null"`
	EnvironmentID                  string           `gorm:"column:environment_id;not null"`
	ImpactDomains                  []string         `gorm:"column:impact_domains;serializer:json;not null;default:'[]'"`
	PrivacyLegalRequired           bool             `gorm:"column:privacy_legal_required;not null;default:false"`
	State                          string           `gorm:"column:state;not null"`
	Version                        int64            `gorm:"column:version;not null;default:1"`
	CreatedBy                      uuid.UUID        `gorm:"column:created_by;type:uuid;not null"`
	DecisionSummary                *string          `gorm:"column:decision_summary"`
	ResidualRiskDisposition        *string          `gorm:"column:residual_risk_disposition"`
	ResidualRisks                  []map[string]any `gorm:"column:residual_risks;serializer:json;not null;default:'[]'"`
	ApprovedAt                     *time.Time       `gorm:"column:approved_at"`
	ReleasedAt                     *time.Time       `gorm:"column:released_at"`
	RejectedAt                     *time.Time       `gorm:"column:rejected_at"`
	RolledBackAt                   *time.Time       `gorm:"column:rolled_back_at"`
	CreatedAt                      time.Time        `gorm:"column:created_at"`
	UpdatedAt                      time.Time        `gorm:"column:updated_at"`
}

func (Stage6ReleaseCandidate) TableName() string { return "stage6_release_candidates" }

type Stage6ReleaseProviderAuthorizationBinding struct {
	CandidateRecordID    uuid.UUID `gorm:"column:candidate_record_id;type:uuid;primaryKey"`
	AuthorizationID      uuid.UUID `gorm:"column:authorization_id;type:uuid;primaryKey"`
	OperatorTenantID     uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Provider             string    `gorm:"column:provider;not null"`
	AuthorizationKey     string    `gorm:"column:authorization_key;not null"`
	AuthorizationVersion int64     `gorm:"column:authorization_version;not null"`
	BoundAt              time.Time `gorm:"column:bound_at;not null"`
}

func (Stage6ReleaseProviderAuthorizationBinding) TableName() string {
	return "stage6_release_provider_authorization_bindings"
}

type Stage6ReleaseApproval struct {
	ID                uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	CandidateRecordID uuid.UUID `gorm:"column:candidate_record_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role              string    `gorm:"column:approval_role;not null"`
	Decision          string    `gorm:"column:decision;not null"`
	ApproverUserID    uuid.UUID `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason            string    `gorm:"column:reason;not null"`
	EvidenceReference string    `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    *string   `gorm:"column:evidence_sha256"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (Stage6ReleaseApproval) TableName() string { return "stage6_release_approvals" }

type Stage6ReleaseFinalReview struct {
	ID                                   uuid.UUID        `gorm:"column:id;type:uuid;primaryKey"`
	CandidateRecordID                    uuid.UUID        `gorm:"column:candidate_record_id;type:uuid;not null;uniqueIndex"`
	OperatorTenantID                     uuid.UUID        `gorm:"column:operator_tenant_id;type:uuid;not null"`
	ReceiptSHA256                        []byte           `gorm:"column:receipt_sha256;not null"`
	Receipt                              []byte           `gorm:"column:receipt;not null"`
	SchemaVersion                        string           `gorm:"column:schema_version;not null"`
	Assessment                           string           `gorm:"column:assessment;not null"`
	ValidatedAt                          time.Time        `gorm:"column:validated_at;not null"`
	ReceiptSizeBytes                     int64            `gorm:"column:receipt_size_bytes;not null"`
	ControlInventorySHA256               string           `gorm:"column:control_inventory_sha256;not null"`
	ControlCount                         int              `gorm:"column:control_count;not null"`
	FinalApprovalCount                   int              `gorm:"column:final_approval_count;not null"`
	DecisionSummary                      string           `gorm:"column:decision_summary;not null"`
	ResidualRiskDisposition              string           `gorm:"column:residual_risk_disposition;not null"`
	ResidualRisks                        []map[string]any `gorm:"column:residual_risks;serializer:json;not null;default:'[]'"`
	AllRequiredControlsPassed            bool             `gorm:"column:all_required_controls_passed;not null"`
	AllRequiredFinalApprovalsApproved    bool             `gorm:"column:all_required_final_approvals_approved;not null"`
	EligibleForExternalGAAuthorityReview bool             `gorm:"column:eligible_for_external_ga_authority_review;not null"`
	ExternalEvidenceAuthorityVerified    bool             `gorm:"column:external_evidence_authority_verified;not null"`
	ApproverCorporateAuthorityVerified   bool             `gorm:"column:approver_corporate_authority_verified;not null"`
	ExternalSignaturesVerified           bool             `gorm:"column:external_signatures_verified;not null"`
	PublicationDeliveryVerifiedBySynara  bool             `gorm:"column:publication_delivery_verified_by_synara;not null"`
	BoundBy                              uuid.UUID        `gorm:"column:bound_by;type:uuid;not null"`
	CreatedAt                            time.Time        `gorm:"column:created_at;not null"`
}

func (Stage6ReleaseFinalReview) TableName() string { return "stage6_release_final_reviews" }
