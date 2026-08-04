package stage6candidate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6capacity"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6incident"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6internalcost"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6operations"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6penetration"
)

var receiptSpecs = map[string][2]string{
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

func Receipt(candidateID, environmentID string) ([]byte, string, error) {
	_, capacityDigest, err := stage6capacity.Receipt(environmentID)
	if err != nil {
		return nil, "", err
	}
	_, penetrationDigest, err := stage6penetration.Receipt(environmentID)
	if err != nil {
		return nil, "", err
	}
	_, incidentDigest, err := stage6incident.Receipt(environmentID)
	if err != nil {
		return nil, "", err
	}
	_, operationsDigest, err := stage6operations.Receipt(candidateID, environmentID)
	if err != nil {
		return nil, "", err
	}
	_, internalCostDigest, err := stage6internalcost.Receipt(
		candidateID, environmentID,
		"000164_session_settlement.sql",
		"sha256:9094065b2ee3881d0a4c83f1b68430d7deec522cc46b557eeec97582f3407b38",
	)
	if err != nil {
		return nil, "", err
	}
	receipts := make(map[string]any, len(receiptSpecs))
	for name, spec := range receiptSpecs {
		receipts[name] = map[string]any{
			"path": name + "-receipt.json", "sha256": digest(name),
			"schemaVersion": spec[0], "assessment": spec[1],
			"readyForCandidateReview": true, "validatedAt": "2026-07-31T11:00:00Z",
		}
	}
	receipts["penetration"].(map[string]any)["sha256"] = penetrationDigest
	receipts["capacity"].(map[string]any)["sha256"] = capacityDigest
	receipts["incident"].(map[string]any)["sha256"] = incidentDigest
	receipts["operations"].(map[string]any)["sha256"] = operationsDigest
	receipts["internalCost"].(map[string]any)["sha256"] = internalCostDigest
	receipts["desktop"].(map[string]any)["desktopArtifactSetSha256"] = "sha256:" + strings.Repeat("e", 64)
	receipts["recovery"].(map[string]any)["candidateBindingSha256"] = "sha256:" + strings.Repeat("a", 64)
	receipts["recovery"].(map[string]any)["recoverySubjectSha256"] = "sha256:" + strings.Repeat("c", 64)
	receipts["recovery"].(map[string]any)["cryptographicSignaturesVerified"] = false
	receipts["recovery"].(map[string]any)["realBackupRestoreAndApproverAuthorityVerificationRequired"] = true
	receipts["residency"].(map[string]any)["candidateBindingSha256"] = "sha256:" + strings.Repeat("a", 64)
	receipts["residency"].(map[string]any)["evidenceSetSha256"] = "sha256:" + strings.Repeat("c", 64)
	receipts["residency"].(map[string]any)["policyDigest"] = "sha256:" + strings.Repeat("d", 64)
	receipts["residency"].(map[string]any)["policyVersion"] = 1
	receipts["residency"].(map[string]any)["homeRegion"] = "region-one"
	receipts["residency"].(map[string]any)["allowedRegions"] = []string{"region-one"}
	receipts["residency"].(map[string]any)["cryptographicSignaturesVerified"] = false
	receipts["residency"].(map[string]any)["externalSignatureIdentityAndAuthorityVerificationRequired"] = true
	receipts["workerSupplyChain"].(map[string]any)["registryReportSha256"] = "sha256:" + strings.Repeat("1", 64)
	receipts["workerSupplyChain"].(map[string]any)["admissionReportSha256"] = "sha256:" + strings.Repeat("2", 64)

	receipt := map[string]any{
		"schemaVersion": "synara.stage6-candidate-evidence-bundle-validation.v5",
		"assessment":    "evidence-consistent-not-ga-approved",
		"candidate": map[string]any{
			"candidateId": candidateID, "sourceCommit": strings.Repeat("a", 40), "lockfileSha256": strings.Repeat("b", 64),
			"desktopArtifactSetSha256": "sha256:" + strings.Repeat("e", 64), "environmentClass": "production-like", "environmentId": environmentID,
			"artifacts": map[string]any{
				"controlPlaneImage": "sha256:" + strings.Repeat("1", 64), "workerImage": "sha256:" + strings.Repeat("2", 64),
				"providerHostImage": "sha256:" + strings.Repeat("3", 64), "webArtifact": "sha256:" + strings.Repeat("4", 64), "adminArtifact": "sha256:" + strings.Repeat("5", 64),
				"desktopArtifacts": map[string]any{
					"linux-x64": "sha256:" + strings.Repeat("6", 64), "macos-arm64": "sha256:" + strings.Repeat("7", 64),
					"macos-x64": "sha256:" + strings.Repeat("8", 64), "windows-x64": "sha256:" + strings.Repeat("9", 64),
				},
			},
			"migrationTail": map[string]any{"name": "000164_session_settlement.sql", "sha256": "sha256:9094065b2ee3881d0a4c83f1b68430d7deec522cc46b557eeec97582f3407b38"},
			"origins":       map[string]any{"controlPlaneBaseUrl": "https://control.example.test/v1", "webBaseUrl": "https://app.example.test", "adminBaseUrl": "https://admin.example.test"},
			"regions":       []string{"region-one"},
		},
		"candidateConsistencyValidated": true, "environmentEligible": true,
		"allRequiredReceiptsReadyForCandidateReview": true, "eligibleForCandidateEvidenceReview": true,
		"requiredReceiptCount": 10, "receipts": receipts, "validatedAt": "2026-07-31T12:00:00Z",
		"manifest": map[string]any{"path": "bundle.json", "sha256": digest("manifest")},
		"compatibilityMatrix": map[string]any{
			"path": "compatibility-matrix.json", "sha256": digest("compatibility-matrix"),
			"schemaVersion": "synara.release-compatibility-matrix.v1", "matrixVersion": 1,
			"assessment": "source-compatible-not-release-approved", "sourceFileCount": 172,
			"sourceByteCount": 1048576,
			"migrationTail":   map[string]any{"name": "000164_session_settlement.sql", "sha256": "sha256:9094065b2ee3881d0a4c83f1b68430d7deec522cc46b557eeec97582f3407b38"},
		},
		"releaseEvidence": map[string]any{
			"path": "release-evidence.json", "sha256": digest("release-evidence"),
			"schemaVersion": "synara-stage6-release-evidence-v2", "assessment": "evidence-collected-not-control-passed", "generatedAt": "2026-07-31T10:00:00Z",
		},
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, "", err
	}
	return encoded, digestBytes(encoded), nil
}

// LegacyV3Receipt returns the exact candidate receipt shape that existed before
// the internal-self-hosted v4 boundary. It is intentionally restricted to
// migration tests which must seed a historical billing-era database before
// applying a later migration; new release candidates must use Receipt.
func LegacyV3Receipt(candidateID, environmentID string) ([]byte, string, error) {
	encoded, _, err := Receipt(candidateID, environmentID)
	if err != nil {
		return nil, "", err
	}
	var receipt map[string]any
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return nil, "", err
	}
	receipt["schemaVersion"] = "synara.stage6-candidate-evidence-bundle-validation.v3"
	delete(receipt, "compatibilityMatrix")
	receipt["candidate"].(map[string]any)["migrationTail"] = map[string]any{
		"name":   "000151_internal_self_hosted_operations_boundary.sql",
		"sha256": "sha256:e1503bd89cc1f8805d202d4967235ac776060e122bf8f4b6d3053fc15bbf4488",
	}
	receipts := receipt["receipts"].(map[string]any)
	_, legacyOperationsDigest, err := stage6operations.LegacyV1Receipt(candidateID, environmentID)
	if err != nil {
		return nil, "", err
	}
	receipts["operations"].(map[string]any)["schemaVersion"] =
		"synara.stage6-operations-browser-exercise-validation.v1"
	receipts["operations"].(map[string]any)["sha256"] = legacyOperationsDigest
	delete(receipts, "internalCost")
	receipts["billing"] = map[string]any{
		"path":                    "billing-receipt.json",
		"sha256":                  digest("billing"),
		"schemaVersion":           "synara.stage6-stripe-billing-exercise-validation.v1",
		"assessment":              "evidence-validated-not-billing-passed",
		"readyForCandidateReview": true,
		"validatedAt":             "2026-07-31T11:00:00Z",
	}
	encoded, err = json.Marshal(receipt)
	if err != nil {
		return nil, "", err
	}
	return encoded, digestBytes(encoded), nil
}

// LegacyV4Receipt returns the Candidate shape accepted through Migration 000162.
// It is only for proving that Migration 000163 retains terminal history while
// refusing to let an active v4 Candidate continue without a compatibility matrix.
func LegacyV4Receipt(candidateID, environmentID string) ([]byte, string, error) {
	encoded, _, err := Receipt(candidateID, environmentID)
	if err != nil {
		return nil, "", err
	}
	var receipt map[string]any
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return nil, "", err
	}
	receipt["schemaVersion"] = "synara.stage6-candidate-evidence-bundle-validation.v4"
	delete(receipt, "compatibilityMatrix")
	receipt["candidate"].(map[string]any)["migrationTail"] = map[string]any{
		"name":   "000162_operations_support_terminal_audit_evidence.sql",
		"sha256": "sha256:ca1952d56c473f03098de3ce09436b74da32f18a9ed51123d8f4c90a3c8d7274",
	}
	encoded, err = json.Marshal(receipt)
	if err != nil {
		return nil, "", err
	}
	return encoded, digestBytes(encoded), nil
}

func digest(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
