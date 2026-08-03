package stage6penetration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

func Receipt(environmentID string) ([]byte, string, error) {
	evidence := map[string]any{}
	for _, name := range []string{
		"stage5CompletionEvidence", "targetRevalidationEvidence", "scopeStatementEvidence",
		"assessorIndependenceEvidence", "executionEvidence", "finalReportEvidence",
		"findingRegisterEvidence", "remediationRetestEvidence", "riskAcceptanceEvidence",
	} {
		evidence[name] = map[string]any{"path": name + ".json", "sha256": digest(name)}
	}
	receipt := map[string]any{
		"schemaVersion":     "synara.third-party-penetration-evidence-receipt.v1",
		"engagementId":      uuid.NewSHA1(uuid.NameSpaceURL, []byte("synara-stage6-penetration/"+environmentID)).String(),
		"releaseCommit":     strings.Repeat("a", 40),
		"environmentClass":  "production-like",
		"environmentId":     environmentID,
		"deploymentProfile": "self-managed-kubernetes",
		"engagement": map[string]any{
			"startedAt": "2026-07-30T00:00:00Z", "completedAt": "2026-07-30T03:00:00Z", "reportIssuedAt": "2026-07-30T04:00:00Z",
		},
		"manifest": map[string]any{"path": "penetration-manifest.json", "sha256": digest("penetration-manifest")},
		"assessor": map[string]any{
			"organizationReference": "assessor/vendor-1", "engagementReference": "contract/pen-2026-01",
			"thirdParty": true, "independent": true, "noConflictDeclared": true,
		},
		"thirdPartyIndependenceDeclared": true,
		"stage5Dependency": map[string]any{
			"status": "accepted-current-supported-surface", "acceptedCommit": strings.Repeat("a", 40),
			"acceptedCommitEqualsReleaseCommit": true, "scopeProfile": "self-managed-kubernetes",
			"scopeProfileMatchesDeployment": true, "targetRevalidated": true, "declaredSatisfied": true,
		},
		"assets": []any{
			map[string]any{"assetType": "control-plane-api", "assetId": "release/control-plane-api", "artifactDigest": "sha256:" + strings.Repeat("1", 64), "tested": true},
			map[string]any{"assetType": "provider-host", "assetId": "release/provider-host", "artifactDigest": "sha256:" + strings.Repeat("3", 64), "tested": true},
			map[string]any{"assetType": "web", "assetId": "release/web", "artifactDigest": "sha256:" + strings.Repeat("4", 64), "tested": true},
			map[string]any{"assetType": "worker-runtime", "assetId": "release/worker-runtime", "artifactDigest": "sha256:" + strings.Repeat("2", 64), "tested": true},
		},
		"assetCoverageComplete": true,
		"scopeCoverage": map[string]any{
			"commandInjection":         map[string]any{"tested": true, "result": "no-finding"},
			"containerEscape":          map[string]any{"tested": true, "result": "no-finding"},
			"crossTenantAuthorization": map[string]any{"tested": true, "result": "no-finding"},
			"pathTraversal":            map[string]any{"tested": true, "result": "no-finding"},
			"ssrf":                     map[string]any{"tested": true, "result": "no-finding"},
			"supplyChain":              map[string]any{"tested": true, "result": "no-finding"},
		},
		"scopeCoverageComplete":       true,
		"methodologies":               []string{"cloud-runtime", "manual-business-logic", "owasp-web-api"},
		"methodologyCoverageComplete": true,
		"findings":                    []any{},
		"findingCounts": map[string]any{
			"bySeverity": map[string]any{"critical": 0, "high": 0, "medium": 0, "low": 0, "informational": 0},
			"byStatus":   map[string]any{"open": 0, "remediation-in-progress": 0, "remediated-verified": 0, "risk-accepted": 0},
		},
		"declaredNoUnacceptedHighOrCriticalFindings": true,
		"evidence":                   evidence,
		"releaseEligibleEnvironment": true,
		"eligibleForHumanGateReview": true,
		"validatedAt":                "2026-07-30T05:00:00Z",
		"assessment":                 "evidence-validated-not-penetration-passed",
	}
	encoded, err := json.Marshal(receipt)
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
