package releasegovernance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	candidateEvidenceReceiptSchema     = "synara.stage6-candidate-evidence-bundle-validation.v5"
	candidateEvidenceReceiptAssessment = "evidence-consistent-not-ga-approved"
	candidateEvidenceReceiptMaxBytes   = 32 * 1024
)

var candidateEvidenceReceiptNames = []string{
	"internalCost", "capacity", "desktop", "incident", "operations",
	"penetration", "recovery", "residency", "slo", "workerSupplyChain",
}

var candidateEvidenceReceiptSpecs = map[string][2]string{
	"internalCost":      {"synara.stage6-internal-cost-evidence-validation.v1", "evidence-validated-not-internal-cost-approved"},
	"capacity":          {"synara.capacity-soak-evidence-receipt.v1", "evidence-validated-not-capacity-passed"},
	"desktop":           {"synara.stage6-desktop-native-acceptance-validation.v1", "evidence-validated-not-desktop-ga-passed"},
	"incident":          {"synara.incident-communication-exercise-evidence-receipt.v2", "evidence-validated-not-operations-ready"},
	"operations":        {"synara.stage6-operations-browser-exercise-validation.v2", "evidence-validated-not-operations-passed"},
	"penetration":       {"synara.third-party-penetration-evidence-receipt.v1", "evidence-validated-not-penetration-passed"},
	"recovery":          {"synara.recovery-drill-evidence-receipt.v2", "evidence-validated-not-control-passed"},
	"residency":         {"synara.data-residency-deployment-evidence-receipt.v1", "evidence-validated-not-residency-approved"},
	"slo":               {"synara.slo-window-evidence-receipt.v1", "evidence-validated-not-slo-passed"},
	"workerSupplyChain": {"synara.stage6-worker-supply-chain-evidence.v1", "evidence-validated-not-worker-supply-chain-approved"},
}

var candidateMigrationNamePattern = regexp.MustCompile(`^[0-9]{6}_[a-z0-9_]+\.sql$`)
var candidateEvidenceIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$`)

type candidateEvidenceBinding struct {
	Receipt                  []byte
	SHA256                   []byte
	Schema                   string
	Assessment               string
	ValidatedAt              string
	ReceiptSizeBytes         int64
	DesktopArtifactSetSHA256 string
}

type candidateEvidenceReceipt struct {
	SchemaVersion                              string                     `json:"schemaVersion"`
	Assessment                                 string                     `json:"assessment"`
	Candidate                                  candidateEvidenceIdentity  `json:"candidate"`
	CandidateConsistencyValidated              bool                       `json:"candidateConsistencyValidated"`
	EnvironmentEligible                        bool                       `json:"environmentEligible"`
	AllRequiredReceiptsReadyForCandidateReview bool                       `json:"allRequiredReceiptsReadyForCandidateReview"`
	EligibleForCandidateEvidenceReview         bool                       `json:"eligibleForCandidateEvidenceReview"`
	RequiredReceiptCount                       int                        `json:"requiredReceiptCount"`
	Receipts                                   map[string]json.RawMessage `json:"receipts"`
	ValidatedAt                                string                     `json:"validatedAt"`
	Manifest                                   json.RawMessage            `json:"manifest"`
	ReleaseEvidence                            json.RawMessage            `json:"releaseEvidence"`
	CompatibilityMatrix                        json.RawMessage            `json:"compatibilityMatrix"`
}

type candidateEvidenceIdentity struct {
	CandidateID              string          `json:"candidateId"`
	SourceCommit             string          `json:"sourceCommit"`
	LockfileSHA256           string          `json:"lockfileSha256"`
	DesktopArtifactSetSHA256 string          `json:"desktopArtifactSetSha256"`
	EnvironmentClass         string          `json:"environmentClass"`
	EnvironmentID            string          `json:"environmentId"`
	Artifacts                json.RawMessage `json:"artifacts"`
	MigrationTail            json.RawMessage `json:"migrationTail"`
	Origins                  json.RawMessage `json:"origins"`
	Regions                  json.RawMessage `json:"regions"`
}

type projectedControlReceipt struct {
	Path                                              string   `json:"path"`
	SHA256                                            string   `json:"sha256"`
	SchemaVersion                                     string   `json:"schemaVersion"`
	Assessment                                        string   `json:"assessment"`
	ReadyForCandidateReview                           *bool    `json:"readyForCandidateReview"`
	ValidatedAt                                       string   `json:"validatedAt"`
	DesktopArtifactSetSHA256                          string   `json:"desktopArtifactSetSha256"`
	CandidateBindingSHA256                            string   `json:"candidateBindingSha256"`
	RecoverySubjectSHA256                             string   `json:"recoverySubjectSha256"`
	CryptographicSignaturesVerified                   *bool    `json:"cryptographicSignaturesVerified"`
	RealBackupRestoreAndApproverAuthorityRequired     *bool    `json:"realBackupRestoreAndApproverAuthorityVerificationRequired"`
	EvidenceSetSHA256                                 string   `json:"evidenceSetSha256"`
	PolicyDigest                                      string   `json:"policyDigest"`
	PolicyVersion                                     *int     `json:"policyVersion"`
	HomeRegion                                        string   `json:"homeRegion"`
	AllowedRegions                                    []string `json:"allowedRegions"`
	ExternalSignatureIdentityAndAuthorityVerification *bool    `json:"externalSignatureIdentityAndAuthorityVerificationRequired"`
	RegistryReportSHA256                              string   `json:"registryReportSha256"`
	AdmissionReportSHA256                             string   `json:"admissionReportSha256"`
}

type projectedCompatibilityMatrix struct {
	Path            string          `json:"path"`
	SHA256          string          `json:"sha256"`
	SchemaVersion   string          `json:"schemaVersion"`
	MatrixVersion   int             `json:"matrixVersion"`
	Assessment      string          `json:"assessment"`
	SourceFileCount int             `json:"sourceFileCount"`
	SourceByteCount int64           `json:"sourceByteCount"`
	MigrationTail   json.RawMessage `json:"migrationTail"`
}

func bindCandidateEvidenceReceipt(input CreateInput, expectedLockfile []byte, now time.Time) (candidateEvidenceBinding, error) {
	encoded := strings.TrimSpace(input.EvidenceBundleReceiptBase64)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(candidateEvidenceReceiptMaxBytes) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate requires a bounded base64 evidence receipt.")
	}
	receiptBytes, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(receiptBytes) == 0 || len(receiptBytes) > candidateEvidenceReceiptMaxBytes || !utf8.Valid(receiptBytes) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence receipt must be bounded UTF-8 JSON.")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(receiptBytes, &raw); err != nil || !exactKeys(raw, []string{
		"allRequiredReceiptsReadyForCandidateReview", "assessment", "candidate",
		"compatibilityMatrix",
		"candidateConsistencyValidated", "eligibleForCandidateEvidenceReview", "environmentEligible",
		"manifest", "receipts", "releaseEvidence", "requiredReceiptCount", "schemaVersion", "validatedAt",
	}) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence receipt schema is invalid.")
	}
	decoder := json.NewDecoder(bytes.NewReader(receiptBytes))
	decoder.DisallowUnknownFields()
	var receipt candidateEvidenceReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence receipt schema is invalid.")
	}
	var candidateRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["candidate"], &candidateRaw); err != nil || !exactKeys(candidateRaw, []string{
		"artifacts", "candidateId", "desktopArtifactSetSha256", "environmentClass", "environmentId",
		"lockfileSha256", "migrationTail", "origins", "regions", "sourceCommit",
	}) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence identity schema is invalid.")
	}
	if receipt.SchemaVersion != candidateEvidenceReceiptSchema || receipt.Assessment != candidateEvidenceReceiptAssessment ||
		!receipt.CandidateConsistencyValidated || !receipt.EnvironmentEligible ||
		!receipt.AllRequiredReceiptsReadyForCandidateReview || !receipt.EligibleForCandidateEvidenceReview ||
		receipt.RequiredReceiptCount != len(candidateEvidenceReceiptNames) ||
		!exactKeys(receipt.Receipts, candidateEvidenceReceiptNames) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence receipt is not eligible for review.")
	}
	if receipt.Candidate.EnvironmentClass != "production" && receipt.Candidate.EnvironmentClass != "production-like" {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence must come from a release-eligible environment.")
	}
	if receipt.Candidate.CandidateID != input.CandidateID || receipt.Candidate.SourceCommit != input.SourceCommit ||
		receipt.Candidate.LockfileSHA256 != hex.EncodeToString(expectedLockfile) ||
		receipt.Candidate.EnvironmentID != input.EnvironmentID {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence identity does not match the candidate.")
	}
	if err := validateCandidateEvidenceIdentity(receipt.Candidate, candidateRaw); err != nil {
		return candidateEvidenceBinding{}, err
	}
	desktopDigest, err := parseSHA256(receipt.Candidate.DesktopArtifactSetSHA256)
	if err != nil || allZero(desktopDigest) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence has an invalid Desktop artifact-set digest.")
	}
	validatedAt, err := time.Parse(time.RFC3339, receipt.ValidatedAt)
	if err != nil || !strings.HasSuffix(receipt.ValidatedAt, "Z") || validatedAt.After(now.Add(5*time.Minute)) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence validatedAt must be a bounded UTC timestamp.")
	}
	if err := validateCandidateEvidenceReferences(receipt, validatedAt); err != nil {
		return candidateEvidenceBinding{}, err
	}

	digest := sha256.Sum256(receiptBytes)
	expectedDigest, err := parseSHA256(input.EvidenceBundleSHA256)
	if err != nil || !bytes.Equal(expectedDigest, digest[:]) || allZero(expectedDigest) {
		return candidateEvidenceBinding{}, invalidCandidateEvidence("Release candidate evidence receipt SHA-256 does not match its bytes.")
	}
	return candidateEvidenceBinding{
		Receipt: slices.Clone(receiptBytes), SHA256: slices.Clone(digest[:]),
		Schema: receipt.SchemaVersion, Assessment: receipt.Assessment,
		ValidatedAt: receipt.ValidatedAt, ReceiptSizeBytes: int64(len(receiptBytes)),
		DesktopArtifactSetSHA256: "sha256:" + hex.EncodeToString(desktopDigest),
	}, nil
}

func validateCandidateEvidenceIdentity(candidate candidateEvidenceIdentity, raw map[string]json.RawMessage) error {
	if !candidateEvidenceIdentifierPattern.MatchString(candidate.CandidateID) || !hex40Pattern.MatchString(candidate.SourceCommit) ||
		!candidateEvidenceIdentifierPattern.MatchString(candidate.EnvironmentID) {
		return invalidCandidateEvidence("Release candidate evidence identity contains an invalid identifier.")
	}
	var artifacts map[string]json.RawMessage
	if err := json.Unmarshal(raw["artifacts"], &artifacts); err != nil || !exactKeys(artifacts, []string{
		"adminArtifact", "controlPlaneImage", "desktopArtifacts", "providerHostImage", "webArtifact", "workerImage",
	}) {
		return invalidCandidateEvidence("Release candidate evidence artifact projection is invalid.")
	}
	for _, key := range []string{"adminArtifact", "controlPlaneImage", "providerHostImage", "webArtifact", "workerImage"} {
		var value string
		if err := json.Unmarshal(artifacts[key], &value); err != nil || !validCanonicalSHA256(value) {
			return invalidCandidateEvidence("Release candidate evidence artifact digest is invalid.")
		}
	}
	var desktopArtifacts map[string]string
	if err := json.Unmarshal(artifacts["desktopArtifacts"], &desktopArtifacts); err != nil || !exactKeys(desktopArtifacts, []string{
		"linux-x64", "macos-arm64", "macos-x64", "windows-x64",
	}) {
		return invalidCandidateEvidence("Release candidate Desktop artifact projection is invalid.")
	}
	for _, value := range desktopArtifacts {
		if !validCanonicalSHA256(value) {
			return invalidCandidateEvidence("Release candidate Desktop artifact digest is invalid.")
		}
	}

	var migration map[string]string
	if err := json.Unmarshal(raw["migrationTail"], &migration); err != nil || !exactKeys(migration, []string{"name", "sha256"}) ||
		!candidateMigrationNamePattern.MatchString(migration["name"]) || !validCanonicalSHA256(migration["sha256"]) {
		return invalidCandidateEvidence("Release candidate migration-tail projection is invalid.")
	}
	var origins map[string]string
	if err := json.Unmarshal(raw["origins"], &origins); err != nil || !exactKeys(origins, []string{
		"adminBaseUrl", "controlPlaneBaseUrl", "webBaseUrl",
	}) || !validCandidateHTTPSURL(origins["controlPlaneBaseUrl"], false) ||
		!validCandidateHTTPSURL(origins["webBaseUrl"], true) || !validCandidateHTTPSURL(origins["adminBaseUrl"], true) ||
		origins["webBaseUrl"] == origins["adminBaseUrl"] {
		return invalidCandidateEvidence("Release candidate origin projection is invalid.")
	}
	var regions []string
	if err := json.Unmarshal(raw["regions"], &regions); err != nil || len(regions) == 0 || !slices.IsSorted(regions) {
		return invalidCandidateEvidence("Release candidate Region projection is invalid.")
	}
	seen := make(map[string]struct{}, len(regions))
	for _, region := range regions {
		if !candidateEvidenceIdentifierPattern.MatchString(region) {
			return invalidCandidateEvidence("Release candidate Region projection is invalid.")
		}
		if _, duplicate := seen[region]; duplicate {
			return invalidCandidateEvidence("Release candidate Regions must be unique.")
		}
		seen[region] = struct{}{}
	}
	return nil
}

func validateCandidateEvidenceReferences(receipt candidateEvidenceReceipt, bundleValidatedAt time.Time) error {
	paths := make(map[string]struct{}, len(candidateEvidenceReceiptNames)+3)
	registerReference := func(raw json.RawMessage, expected []string, label string) (map[string]json.RawMessage, error) {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil || !exactKeys(value, expected) {
			return nil, invalidCandidateEvidence(label + " projection is invalid.")
		}
		var path, digest string
		if err := json.Unmarshal(value["path"], &path); err != nil || !validCandidateEvidencePath(path) {
			return nil, invalidCandidateEvidence(label + " path is invalid.")
		}
		if err := json.Unmarshal(value["sha256"], &digest); err != nil || !validCanonicalSHA256(digest) {
			return nil, invalidCandidateEvidence(label + " digest is invalid.")
		}
		if _, duplicate := paths[path]; duplicate {
			return nil, invalidCandidateEvidence("Release candidate evidence paths must be unique.")
		}
		paths[path] = struct{}{}
		return value, nil
	}
	if _, err := registerReference(receipt.Manifest, []string{"path", "sha256"}, "Release candidate manifest"); err != nil {
		return err
	}
	compatibilityRaw, err := registerReference(receipt.CompatibilityMatrix, []string{
		"assessment", "matrixVersion", "migrationTail", "path", "schemaVersion", "sha256",
		"sourceByteCount", "sourceFileCount",
	}, "Release compatibility matrix")
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(receipt.CompatibilityMatrix))
	decoder.DisallowUnknownFields()
	var compatibility projectedCompatibilityMatrix
	if err := decoder.Decode(&compatibility); err != nil ||
		compatibility.SchemaVersion != "synara.release-compatibility-matrix.v1" ||
		compatibility.MatrixVersion != 1 ||
		compatibility.Assessment != "source-compatible-not-release-approved" ||
		compatibility.SourceFileCount < 1 || compatibility.SourceByteCount < 1 {
		return invalidCandidateEvidence("Release compatibility matrix projection is invalid.")
	}
	var compatibilityMigration, candidateMigration map[string]string
	if err := json.Unmarshal(compatibilityRaw["migrationTail"], &compatibilityMigration); err != nil ||
		json.Unmarshal(receipt.Candidate.MigrationTail, &candidateMigration) != nil ||
		!exactKeys(compatibilityMigration, []string{"name", "sha256"}) ||
		!maps.Equal(compatibilityMigration, candidateMigration) {
		return invalidCandidateEvidence("Release compatibility matrix migration tail does not match the candidate.")
	}
	releaseEvidence, err := registerReference(receipt.ReleaseEvidence, []string{
		"assessment", "generatedAt", "path", "schemaVersion", "sha256",
	}, "Release evidence")
	if err != nil {
		return err
	}
	var releaseSchema, releaseAssessment, generatedAtRaw string
	if json.Unmarshal(releaseEvidence["schemaVersion"], &releaseSchema) != nil || releaseSchema != "synara-stage6-release-evidence-v2" ||
		json.Unmarshal(releaseEvidence["assessment"], &releaseAssessment) != nil || releaseAssessment != "evidence-collected-not-control-passed" ||
		json.Unmarshal(releaseEvidence["generatedAt"], &generatedAtRaw) != nil {
		return invalidCandidateEvidence("Release evidence projection is invalid.")
	}
	generatedAt, err := time.Parse(time.RFC3339, generatedAtRaw)
	if err != nil || !strings.HasSuffix(generatedAtRaw, "Z") || generatedAt.After(bundleValidatedAt) {
		return invalidCandidateEvidence("Release evidence generatedAt is invalid.")
	}

	var candidateRegions []string
	if err := json.Unmarshal(receipt.Candidate.Regions, &candidateRegions); err != nil {
		return invalidCandidateEvidence("Release candidate Region projection is invalid.")
	}
	for _, name := range candidateEvidenceReceiptNames {
		raw := receipt.Receipts[name]
		keys := []string{"assessment", "path", "readyForCandidateReview", "schemaVersion", "sha256", "validatedAt"}
		switch name {
		case "desktop":
			keys = append(keys, "desktopArtifactSetSha256")
		case "recovery":
			keys = append(keys, "candidateBindingSha256", "cryptographicSignaturesVerified", "realBackupRestoreAndApproverAuthorityVerificationRequired", "recoverySubjectSha256")
		case "residency":
			keys = append(keys, "allowedRegions", "candidateBindingSha256", "cryptographicSignaturesVerified", "evidenceSetSha256", "externalSignatureIdentityAndAuthorityVerificationRequired", "homeRegion", "policyDigest", "policyVersion")
		case "workerSupplyChain":
			keys = append(keys, "admissionReportSha256", "registryReportSha256")
		}
		if _, err := registerReference(raw, keys, "Release candidate "+name+" receipt"); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var projection projectedControlReceipt
		if err := decoder.Decode(&projection); err != nil {
			return invalidCandidateEvidence("Release candidate " + name + " receipt projection is invalid.")
		}
		spec := candidateEvidenceReceiptSpecs[name]
		validatedAt, err := time.Parse(time.RFC3339, projection.ValidatedAt)
		if projection.SchemaVersion != spec[0] || projection.Assessment != spec[1] || projection.ReadyForCandidateReview == nil || !*projection.ReadyForCandidateReview ||
			err != nil || !strings.HasSuffix(projection.ValidatedAt, "Z") || validatedAt.After(bundleValidatedAt) {
			return invalidCandidateEvidence("Release candidate " + name + " receipt is not eligible for review.")
		}
		switch name {
		case "desktop":
			if projection.DesktopArtifactSetSHA256 != receipt.Candidate.DesktopArtifactSetSHA256 || !validCanonicalSHA256(projection.DesktopArtifactSetSHA256) {
				return invalidCandidateEvidence("Release candidate Desktop artifact-set projection is inconsistent.")
			}
		case "recovery":
			if !validCanonicalSHA256(projection.CandidateBindingSHA256) || !validCanonicalSHA256(projection.RecoverySubjectSHA256) ||
				projection.CryptographicSignaturesVerified == nil || *projection.CryptographicSignaturesVerified ||
				projection.RealBackupRestoreAndApproverAuthorityRequired == nil || !*projection.RealBackupRestoreAndApproverAuthorityRequired {
				return invalidCandidateEvidence("Release candidate recovery verification boundary is invalid.")
			}
		case "residency":
			if !validCanonicalSHA256(projection.CandidateBindingSHA256) || !validCanonicalSHA256(projection.EvidenceSetSHA256) ||
				!validCanonicalSHA256(projection.PolicyDigest) || projection.PolicyVersion == nil || *projection.PolicyVersion < 1 ||
				!candidateEvidenceIdentifierPattern.MatchString(projection.HomeRegion) || !slices.Equal(projection.AllowedRegions, candidateRegions) ||
				projection.CryptographicSignaturesVerified == nil || *projection.CryptographicSignaturesVerified ||
				projection.ExternalSignatureIdentityAndAuthorityVerification == nil || !*projection.ExternalSignatureIdentityAndAuthorityVerification {
				return invalidCandidateEvidence("Release candidate residency verification boundary is invalid.")
			}
		case "workerSupplyChain":
			if !validCanonicalSHA256(projection.RegistryReportSHA256) || !validCanonicalSHA256(projection.AdmissionReportSHA256) {
				return invalidCandidateEvidence("Release candidate Worker supply-chain evidence projection is invalid.")
			}
		}
	}
	return nil
}

func validCandidateEvidencePath(value string) bool {
	if value == "" || len(value) > 512 || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\r\n\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validCanonicalSHA256(value string) bool {
	if len(value) != 71 || value != strings.ToLower(value) || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && !allZero(digest)
}

func validCandidateHTTPSURL(value string, originOnly bool) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return !originOnly || parsed.Path == ""
}

func exactKeys[T any](values map[string]T, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func allZero(value []byte) bool {
	if len(value) == 0 {
		return true
	}
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func invalidCandidateEvidence(detail string) error {
	return problem.New(400, "release_candidate_evidence_invalid", detail)
}
