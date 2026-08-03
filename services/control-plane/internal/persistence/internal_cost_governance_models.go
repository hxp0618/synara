package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6InternalCostReview retains one exact-candidate internal usage, Token and cost receipt.
// It contains no payment, subscription, invoice, tax or card-processing state.
type Stage6InternalCostReview struct {
	ID                                             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperatorTenantID                               uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	CandidateRecordID                              uuid.UUID  `gorm:"column:candidate_record_id;type:uuid;not null"`
	Receipt                                        []byte     `gorm:"column:receipt;not null"`
	ReceiptSHA256                                  []byte     `gorm:"column:receipt_sha256;not null"`
	ReceiptSizeBytes                               int64      `gorm:"column:receipt_size_bytes;not null"`
	Schema                                         string     `gorm:"column:receipt_schema;not null"`
	Assessment                                     string     `gorm:"column:assessment;not null"`
	ReleaseCommit                                  string     `gorm:"column:release_commit;not null"`
	EnvironmentClass                               string     `gorm:"column:environment_class;not null"`
	EnvironmentID                                  string     `gorm:"column:environment_id;not null"`
	ManifestSHA256                                 []byte     `gorm:"column:manifest_sha256;not null"`
	ControlPlaneOrigin                             string     `gorm:"column:control_plane_origin;not null"`
	MigrationName                                  string     `gorm:"column:migration_name;not null"`
	MigrationSHA256                                []byte     `gorm:"column:migration_sha256;not null"`
	PeriodStart                                    time.Time  `gorm:"column:period_start;not null"`
	PeriodEnd                                      time.Time  `gorm:"column:period_end;not null"`
	ValidatedAt                                    time.Time  `gorm:"column:validated_at;not null"`
	ExecutionCount                                 int64      `gorm:"column:execution_count;not null"`
	InputTokens                                    int64      `gorm:"column:input_tokens;not null"`
	OutputTokens                                   int64      `gorm:"column:output_tokens;not null"`
	CachedInputTokens                              int64      `gorm:"column:cached_input_tokens;not null"`
	CacheCreationInputTokens                       int64      `gorm:"column:cache_creation_input_tokens;not null"`
	ProviderCostReportedExecutionCount             int64      `gorm:"column:provider_cost_reported_execution_count;not null"`
	ProviderCostUnavailableExecutionCount          int64      `gorm:"column:provider_cost_unavailable_execution_count;not null"`
	ActualPlatformAllocationCount                  int64      `gorm:"column:actual_platform_allocation_count;not null"`
	EstimatedPlatformAllocationCount               int64      `gorm:"column:estimated_platform_allocation_count;not null"`
	EvidenceFileCount                              int64      `gorm:"column:evidence_file_count;not null"`
	TokenTotalsReconciled                          bool       `gorm:"column:token_totals_reconciled;not null"`
	ProviderCoverageComplete                       bool       `gorm:"column:provider_coverage_complete;not null"`
	ActualOverridesEstimate                        bool       `gorm:"column:actual_overrides_estimate;not null"`
	CurrencySafeAggregation                        bool       `gorm:"column:currency_safe_aggregation;not null"`
	TenantIsolationValidated                       bool       `gorm:"column:tenant_isolation_validated;not null"`
	NoPaymentDataPresent                           bool       `gorm:"column:no_payment_data_present;not null"`
	EligibleForHumanGateReview                     bool       `gorm:"column:eligible_for_human_gate_review;not null"`
	CryptographicSignaturesVerified                bool       `gorm:"column:cryptographic_signatures_verified;not null"`
	ExternalSourceAndAuthorityVerificationRequired bool       `gorm:"column:external_source_authority_verification_required;not null"`
	State                                          string     `gorm:"column:state;not null"`
	Version                                        int64      `gorm:"column:version;not null;default:1"`
	CreatedBy                                      uuid.UUID  `gorm:"column:created_by;type:uuid;not null"`
	ApprovedAt                                     *time.Time `gorm:"column:approved_at"`
	RejectedAt                                     *time.Time `gorm:"column:rejected_at"`
	CreatedAt                                      time.Time  `gorm:"column:created_at"`
	UpdatedAt                                      time.Time  `gorm:"column:updated_at"`
}

func (Stage6InternalCostReview) TableName() string { return "stage6_internal_cost_reviews" }

type Stage6InternalCostReviewApproval struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	InternalCostID    uuid.UUID  `gorm:"column:internal_cost_review_id;type:uuid;not null"`
	OperatorTenantID  uuid.UUID  `gorm:"column:operator_tenant_id;type:uuid;not null"`
	Role              string     `gorm:"column:approval_role;not null"`
	Decision          string     `gorm:"column:decision;not null"`
	ApproverUserID    uuid.UUID  `gorm:"column:approver_user_id;type:uuid;not null"`
	Reason            string     `gorm:"column:reason;not null"`
	EvidenceReference string     `gorm:"column:evidence_reference;not null"`
	EvidenceSHA256    string     `gorm:"column:evidence_sha256;not null"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	SupersededAt      *time.Time `gorm:"column:superseded_at"`
	SupersededReason  *string    `gorm:"column:superseded_reason"`
}

func (Stage6InternalCostReviewApproval) TableName() string {
	return "stage6_internal_cost_review_approvals"
}
