package persistence

import (
	"time"

	"github.com/google/uuid"
)

// Stage6BillingExercise retains the exact deployed Stripe billing
// receipt referenced by one release candidate. Internal approval does not
// authenticate Stripe, settlement, tax, evidence files, signatures, execution,
// or external approver authority.
type Stage6BillingExercise struct {
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
	ManifestSHA256                        []byte     `gorm:"column:manifest_sha256;not null"`
	ControlPlaneOrigin                    string     `gorm:"column:control_plane_origin;not null"`
	StripeMode                            string     `gorm:"column:stripe_mode;not null"`
	StripeAPIVersion                      string     `gorm:"column:stripe_api_version;not null"`
	MigrationName                         string     `gorm:"column:migration_name;not null"`
	MigrationSHA256                       []byte     `gorm:"column:migration_sha256;not null"`
	StartedAt                             time.Time  `gorm:"column:started_at;not null"`
	CompletedAt                           time.Time  `gorm:"column:completed_at;not null"`
	ValidatedAt                           time.Time  `gorm:"column:validated_at;not null"`
	ScenarioCount                         int64      `gorm:"column:scenario_count;not null"`
	EvidenceFileCount                     int64      `gorm:"column:evidence_file_count;not null"`
	AllScenariosPassed                    bool       `gorm:"column:all_scenarios_passed;not null"`
	Currency                              string     `gorm:"column:currency;not null"`
	ExpectedAmountMinor                   int64      `gorm:"column:expected_amount_minor;not null"`
	InvoiceAmountMinor                    int64      `gorm:"column:invoice_amount_minor;not null"`
	SettledAmountMinor                    int64      `gorm:"column:settled_amount_minor;not null"`
	ExpectedTaxMinor                      int64      `gorm:"column:expected_tax_minor;not null"`
	InvoiceTaxMinor                       int64      `gorm:"column:invoice_tax_minor;not null"`
	ChargeCount                           int64      `gorm:"column:charge_count;not null"`
	SubscriptionCount                     int64      `gorm:"column:subscription_count;not null"`
	DuplicateChargeCount                  int64      `gorm:"column:duplicate_charge_count;not null"`
	RawWebhookPayloadStored               bool       `gorm:"column:raw_webhook_payload_stored;not null"`
	CardDataHandledBySynara               bool       `gorm:"column:card_data_handled_by_synara;not null"`
	AmountsMatch                          bool       `gorm:"column:amounts_match;not null"`
	CardinalityMatches                    bool       `gorm:"column:cardinality_matches;not null"`
	ReceiptApprovalsComplete              bool       `gorm:"column:receipt_approvals_complete;not null"`
	LiveMode                              bool       `gorm:"column:live_mode;not null"`
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

func (Stage6BillingExercise) TableName() string { return "stage6_billing_exercises" }

type Stage6BillingExerciseApproval struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	BillingExerciseID uuid.UUID  `gorm:"column:billing_exercise_id;type:uuid;not null"`
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

func (Stage6BillingExerciseApproval) TableName() string {
	return "stage6_billing_exercise_approvals"
}
