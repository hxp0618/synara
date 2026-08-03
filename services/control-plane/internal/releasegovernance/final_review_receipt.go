package releasegovernance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	finalReviewReceiptSchema     = "synara.stage6-final-ga-review-validation.v1"
	finalReviewReceiptAssessment = "final-review-consistent-not-ga-authority-verified"
	maxFinalReviewReceiptBytes   = 512 * 1024
)

var finalReviewControlOwners = map[string]string{
	"tenant-registration":          "product",
	"tenant-lifecycle":             "operations",
	"identity-lifecycle":           "security",
	"sso-domain":                   "security",
	"identity-governance":          "security",
	"offer-admission":              "product",
	"usage-reconciliation":         "engineering",
	"quota-concurrency":            "engineering",
	"usage-explainability":         "product",
	"internal-cost-reconciliation": "operations",
	"operations-matrix":            "operations",
	"authority-views":              "operations",
	"support-access":               "security",
	"incident-communications":      "operations",
	"audit-retention-legal-hold":   "security",
	"privacy-workflows":            "privacy_legal",
	"data-residency":               "privacy_legal",
	"provider-compliance":          "privacy_legal",
	"compliance-program":           "security",
	"desktop-native":               "engineering",
	"desktop-security":             "security",
	"desktop-postgres-concurrency": "engineering",
	"recovery":                     "operations",
	"slo":                          "operations",
	"tracing-isolation":            "security",
	"penetration-stage5":           "security",
	"worker-supply-chain":          "security",
	"compatibility-rollback":       "engineering",
	"key-rotation":                 "security",
	"capacity":                     "operations",
	"documentation":                "product",
	"change-notice":                "product",
}

type finalReviewReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type finalReviewCandidate struct {
	CandidateID                string               `json:"candidateId"`
	ReleaseTag                 string               `json:"releaseTag"`
	SourceCommit               string               `json:"sourceCommit"`
	EnvironmentID              string               `json:"environmentId"`
	CandidateBundleReceipt     finalReviewReference `json:"candidateBundleReceipt"`
	ProtectedReleaseApproval   finalReviewReference `json:"protectedReleaseApproval"`
	FinalAssetSet              finalReviewReference `json:"finalAssetSet"`
	FinalAssetSetSHA256        string               `json:"finalAssetSetSha256"`
	LockfileSHA256             string               `json:"lockfileSha256"`
	DesktopArtifactSetSHA256   string               `json:"desktopArtifactSetSha256"`
	CopiedChecklist            finalReviewReference `json:"copiedChecklist"`
	ChangeNotice               finalReviewReference `json:"changeNotice"`
	PlatformAuditExport        finalReviewReference `json:"platformAuditExport"`
	CandidateValidatedAt       string               `json:"candidateValidatedAt"`
	ProtectedReleaseApprovedAt string               `json:"protectedReleaseApprovedAt"`
}

type finalReviewStatusCounts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Blocked int `json:"blocked"`
}

type finalReviewControlDecision struct {
	ID         string                 `json:"id"`
	OwnerRole  string                 `json:"ownerRole"`
	ApproverID string                 `json:"approverId"`
	Status     string                 `json:"status"`
	DecidedAt  string                 `json:"decidedAt"`
	Evidence   []finalReviewReference `json:"evidence"`
}

type finalReviewApproval struct {
	Role       string               `json:"role"`
	ApproverID string               `json:"approverId"`
	Decision   string               `json:"decision"`
	ApprovedAt string               `json:"approvedAt"`
	Evidence   finalReviewReference `json:"evidence"`
}

type finalReviewBoundary struct {
	ExternalEvidenceAuthorityVerified   bool `json:"externalEvidenceAuthorityVerified"`
	ApproverCorporateAuthorityVerified  bool `json:"approverCorporateAuthorityVerified"`
	ExternalSignaturesVerified          bool `json:"externalSignaturesVerified"`
	PublicationDeliveryVerifiedBySynara bool `json:"publicationDeliveryVerifiedBySynara"`
}

type finalReviewReceipt struct {
	SchemaVersion                        string                       `json:"schemaVersion"`
	Candidate                            finalReviewCandidate         `json:"candidate"`
	ReleaseManagerID                     string                       `json:"releaseManagerId"`
	ImpactDomains                        []string                     `json:"impactDomains"`
	ControlInventorySHA256               string                       `json:"controlInventorySha256"`
	ControlCount                         int                          `json:"controlCount"`
	ControlStatusCounts                  finalReviewStatusCounts      `json:"controlStatusCounts"`
	ControlDecisions                     []finalReviewControlDecision `json:"controlDecisions"`
	FinalApprovals                       []finalReviewApproval        `json:"finalApprovals"`
	PlatformAuditRequestIDs              []string                     `json:"platformAuditRequestIds"`
	DecisionSummary                      string                       `json:"decisionSummary"`
	ResidualRiskDisposition              string                       `json:"residualRiskDisposition"`
	ResidualRisks                        []ResidualRisk               `json:"residualRisks"`
	StartedAt                            string                       `json:"startedAt"`
	CompletedAt                          string                       `json:"completedAt"`
	ValidatedAt                          string                       `json:"validatedAt"`
	AllRequiredControlsPassed            bool                         `json:"allRequiredControlsPassed"`
	AllRequiredFinalApprovalsApproved    bool                         `json:"allRequiredFinalApprovalsApproved"`
	EligibleForExternalGAAuthorityReview bool                         `json:"eligibleForExternalGAAuthorityReview"`
	VerificationBoundary                 finalReviewBoundary          `json:"verificationBoundary"`
	Assessment                           string                       `json:"assessment"`
}

func bindFinalReviewReceipt(
	input FinalReviewInput,
	candidate persistence.Stage6ReleaseCandidate,
	actorID uuid.UUID,
	now time.Time,
) (persistence.Stage6ReleaseFinalReview, error) {
	receipt, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(input.ReceiptBase64))
	if err != nil || len(receipt) < 1 || len(receipt) > maxFinalReviewReceiptBytes {
		return persistence.Stage6ReleaseFinalReview{}, problem.New(400, "release_final_review_invalid", "Final review receipt must be bounded canonical base64.")
	}
	expectedSHA256, err := parseSHA256(input.ReceiptSHA256)
	if err != nil {
		return persistence.Stage6ReleaseFinalReview{}, err
	}
	actualSHA256 := sha256.Sum256(receipt)
	if !bytes.Equal(expectedSHA256, actualSHA256[:]) {
		return persistence.Stage6ReleaseFinalReview{}, problem.New(400, "release_final_review_digest_mismatch", "Final review receipt digest does not match its exact bytes.")
	}
	decoder := json.NewDecoder(bytes.NewReader(receipt))
	decoder.DisallowUnknownFields()
	var document finalReviewReceipt
	if err := decoder.Decode(&document); err != nil {
		return persistence.Stage6ReleaseFinalReview{}, problem.New(400, "release_final_review_invalid", "Final review receipt does not match the v1 schema.")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return persistence.Stage6ReleaseFinalReview{}, problem.New(400, "release_final_review_invalid", "Final review receipt must contain one JSON object.")
	}
	if err := validateFinalReviewReceipt(document, candidate, now); err != nil {
		return persistence.Stage6ReleaseFinalReview{}, err
	}
	validatedAt, _ := time.Parse(time.RFC3339, document.ValidatedAt)
	return persistence.Stage6ReleaseFinalReview{
		ID: uuid.New(), CandidateRecordID: candidate.ID, OperatorTenantID: candidate.OperatorTenantID,
		ReceiptSHA256: actualSHA256[:], Receipt: slices.Clone(receipt), SchemaVersion: document.SchemaVersion,
		Assessment: document.Assessment, ValidatedAt: validatedAt.UTC(), ReceiptSizeBytes: int64(len(receipt)),
		ControlInventorySHA256: document.ControlInventorySHA256, ControlCount: document.ControlCount,
		FinalApprovalCount: len(document.FinalApprovals), DecisionSummary: document.DecisionSummary,
		ResidualRiskDisposition:              document.ResidualRiskDisposition,
		ResidualRisks:                        risksToPersistence(document.ResidualRisks),
		AllRequiredControlsPassed:            document.AllRequiredControlsPassed,
		AllRequiredFinalApprovalsApproved:    document.AllRequiredFinalApprovalsApproved,
		EligibleForExternalGAAuthorityReview: document.EligibleForExternalGAAuthorityReview,
		ExternalEvidenceAuthorityVerified:    document.VerificationBoundary.ExternalEvidenceAuthorityVerified,
		ApproverCorporateAuthorityVerified:   document.VerificationBoundary.ApproverCorporateAuthorityVerified,
		ExternalSignaturesVerified:           document.VerificationBoundary.ExternalSignaturesVerified,
		PublicationDeliveryVerifiedBySynara:  document.VerificationBoundary.PublicationDeliveryVerifiedBySynara,
		BoundBy:                              actorID, CreatedAt: now,
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var value any
	if err := decoder.Decode(&value); err != io.EOF {
		return err
	}
	return nil
}

func validateFinalReviewReceipt(
	document finalReviewReceipt,
	candidate persistence.Stage6ReleaseCandidate,
	now time.Time,
) error {
	invalid := func() error {
		return problem.New(400, "release_final_review_invalid", "Final review receipt is not an eligible exact-candidate v1 archive.")
	}
	if document.SchemaVersion != finalReviewReceiptSchema || document.Assessment != finalReviewReceiptAssessment ||
		!document.AllRequiredControlsPassed || !document.AllRequiredFinalApprovalsApproved ||
		!document.EligibleForExternalGAAuthorityReview || document.ControlCount != len(finalReviewControlOwners) ||
		document.ControlStatusCounts.Passed != len(finalReviewControlOwners) || document.ControlStatusCounts.Failed != 0 ||
		document.ControlStatusCounts.Blocked != 0 || len(document.ControlDecisions) != len(finalReviewControlOwners) ||
		document.VerificationBoundary.ExternalEvidenceAuthorityVerified ||
		document.VerificationBoundary.ApproverCorporateAuthorityVerified ||
		document.VerificationBoundary.ExternalSignaturesVerified ||
		document.VerificationBoundary.PublicationDeliveryVerifiedBySynara || document.ResidualRisks == nil {
		return invalid()
	}
	canonicalOwners, _ := json.Marshal(finalReviewControlOwners)
	controlDigest := sha256.Sum256(canonicalOwners)
	if document.ControlInventorySHA256 != "sha256:"+hex.EncodeToString(controlDigest[:]) {
		return invalid()
	}
	if document.Candidate.CandidateID != candidate.CandidateID || document.Candidate.ReleaseTag != candidate.CandidateID ||
		document.Candidate.SourceCommit != candidate.SourceCommit || document.Candidate.EnvironmentID != candidate.EnvironmentID ||
		document.Candidate.LockfileSHA256 != hex.EncodeToString(candidate.LockfileSHA256) ||
		document.Candidate.DesktopArtifactSetSHA256 != candidate.DesktopArtifactSetSHA256 ||
		document.Candidate.FinalAssetSetSHA256 != formatSHA256(candidate.FinalAssetSetSHA256) ||
		document.Candidate.CandidateBundleReceipt.SHA256 != formatSHA256(candidate.EvidenceBundleSHA256) {
		return invalid()
	}
	for _, reference := range []finalReviewReference{
		document.Candidate.CandidateBundleReceipt, document.Candidate.ProtectedReleaseApproval,
		document.Candidate.FinalAssetSet, document.Candidate.CopiedChecklist,
		document.Candidate.ChangeNotice, document.Candidate.PlatformAuditExport,
	} {
		if !validFinalReviewReference(reference) {
			return invalid()
		}
	}
	domains := slices.Clone(document.ImpactDomains)
	slices.Sort(domains)
	if !slices.Equal(domains, candidate.ImpactDomains) || len(domains) != len(document.ImpactDomains) {
		return invalid()
	}
	startedAt, err1 := time.Parse(time.RFC3339, document.StartedAt)
	completedAt, err2 := time.Parse(time.RFC3339, document.CompletedAt)
	validatedAt, err3 := time.Parse(time.RFC3339, document.ValidatedAt)
	candidateValidatedAt, err4 := time.Parse(time.RFC3339, document.Candidate.CandidateValidatedAt)
	protectedApprovedAt, err5 := time.Parse(time.RFC3339, document.Candidate.ProtectedReleaseApprovedAt)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil ||
		!completedAt.After(startedAt) || validatedAt.Before(completedAt) || validatedAt.After(now.Add(5*time.Minute)) ||
		candidateValidatedAt.After(protectedApprovedAt) || protectedApprovedAt.After(completedAt) ||
		document.Candidate.CandidateValidatedAt != candidate.EvidenceBundleValidatedAt {
		return invalid()
	}
	seenControls := make(map[string]struct{}, len(document.ControlDecisions))
	latestControlDecision := startedAt
	for _, control := range document.ControlDecisions {
		owner, exists := finalReviewControlOwners[control.ID]
		decidedAt, timeErr := time.Parse(time.RFC3339, control.DecidedAt)
		if !exists || owner != control.OwnerRole || control.Status != "passed" ||
			!boundedFinalIdentity(control.ApproverID) || timeErr != nil || decidedAt.Before(startedAt) ||
			decidedAt.After(completedAt) || len(control.Evidence) < 1 {
			return invalid()
		}
		if _, duplicate := seenControls[control.ID]; duplicate {
			return invalid()
		}
		seenControls[control.ID] = struct{}{}
		if decidedAt.After(latestControlDecision) {
			latestControlDecision = decidedAt
		}
		for _, reference := range control.Evidence {
			if !validFinalReviewReference(reference) {
				return invalid()
			}
		}
	}
	requiredRoles := requiredApprovalRolesFor(candidate)
	seenRoles := make(map[string]struct{}, len(document.FinalApprovals))
	seenApprovers := map[string]struct{}{document.ReleaseManagerID: {}}
	for _, approval := range document.FinalApprovals {
		approvedAt, timeErr := time.Parse(time.RFC3339, approval.ApprovedAt)
		roleRequired := slices.Contains(requiredRoles, approval.Role)
		if !roleRequired || approval.Decision != "approved-for-ga-authority-review" ||
			!boundedFinalIdentity(approval.ApproverID) || timeErr != nil || approvedAt.Before(latestControlDecision) ||
			approvedAt.Before(protectedApprovedAt) || approvedAt.After(completedAt) || !validFinalReviewReference(approval.Evidence) {
			return invalid()
		}
		if _, duplicate := seenRoles[approval.Role]; duplicate {
			return invalid()
		}
		if _, duplicate := seenApprovers[approval.ApproverID]; duplicate {
			return invalid()
		}
		seenRoles[approval.Role] = struct{}{}
		seenApprovers[approval.ApproverID] = struct{}{}
	}
	if len(seenRoles) != len(requiredRoles) || !boundedFinalIdentity(document.ReleaseManagerID) ||
		len(document.PlatformAuditRequestIDs) < 1 || len(strings.TrimSpace(document.DecisionSummary)) < 20 ||
		len(strings.TrimSpace(document.DecisionSummary)) > 4000 {
		return invalid()
	}
	seenRequests := make(map[uuid.UUID]struct{}, len(document.PlatformAuditRequestIDs))
	for _, raw := range document.PlatformAuditRequestIDs {
		requestID, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			return invalid()
		}
		if _, duplicate := seenRequests[requestID]; duplicate {
			return invalid()
		}
		seenRequests[requestID] = struct{}{}
	}
	if err := validateFinalReviewRisks(document.ResidualRiskDisposition, document.ResidualRisks, completedAt); err != nil {
		return invalid()
	}
	return nil
}

func validFinalReviewReference(reference finalReviewReference) bool {
	if len(reference.Path) < 1 || len(reference.Path) > 512 || strings.Contains(reference.Path, "\\") ||
		path.IsAbs(reference.Path) || path.Clean(reference.Path) != reference.Path || strings.HasPrefix(reference.Path, "../") ||
		len(reference.SHA256) != 71 || !strings.HasPrefix(reference.SHA256, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	_, err := hex.DecodeString(digest)
	return err == nil && digest != strings.Repeat("0", 64)
}

func boundedFinalIdentity(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 2 && len(value) <= 200 && !strings.ContainsAny(value, "\r\n\x00")
}

func validateFinalReviewRisks(disposition string, risks []ResidualRisk, completedAt time.Time) error {
	if disposition == "none" && len(risks) == 0 {
		return nil
	}
	if disposition != "accepted" || len(risks) < 1 || len(risks) > 50 {
		return problem.New(400, "release_final_review_invalid", "Final review residual-risk disposition is invalid.")
	}
	seen := make(map[string]struct{}, len(risks))
	for _, risk := range risks {
		if _, duplicate := seen[risk.ID]; duplicate || !riskIDPattern.MatchString(risk.ID) ||
			len(strings.TrimSpace(risk.Summary)) < 10 || len(strings.TrimSpace(risk.Summary)) > 500 ||
			len(strings.TrimSpace(risk.Owner)) < 3 || len(strings.TrimSpace(risk.Owner)) > 200 ||
			len(strings.TrimSpace(risk.AcceptanceReason)) < 10 || len(strings.TrimSpace(risk.AcceptanceReason)) > 1000 ||
			!risk.DueAt.After(completedAt) || !validEvidenceReference(risk.EvidenceReference) {
			return problem.New(400, "release_final_review_invalid", "Final review residual risk is invalid.")
		}
		seen[risk.ID] = struct{}{}
	}
	return nil
}
