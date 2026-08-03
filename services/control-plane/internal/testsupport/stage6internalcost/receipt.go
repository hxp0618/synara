package stage6internalcost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func Receipt(candidateID, environmentID, migrationName, migrationSHA256 string) ([]byte, string, error) {
	evidenceIDs := []string{"platform-allocation-export", "provider-cost-export", "reconciliation-report", "usage-export"}
	evidence := make([]map[string]any, 0, len(evidenceIDs))
	for _, id := range evidenceIDs {
		evidence = append(evidence, map[string]any{"id": id, "path": id + ".json", "sha256": digest(id)})
	}
	receipt := map[string]any{
		"schemaVersion": "synara.stage6-internal-cost-evidence-validation.v1",
		"assessment":    "evidence-validated-not-internal-cost-approved",
		"candidate": map[string]any{
			"candidateId": candidateID, "sourceCommit": strings.Repeat("a", 40),
			"environment": "production-like", "environmentId": environmentID,
			"controlPlaneBaseUrl": "https://control.example.test/v1",
			"migrationTail":       map[string]any{"name": migrationName, "sha256": migrationSHA256},
		},
		"period": map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-31T00:00:00Z"},
		"usage": map[string]any{
			"executionCount": 2, "inputTokens": 1000, "outputTokens": 250,
			"cachedInputTokens": 300, "cacheCreationInputTokens": 100,
			"providerCostReportedExecutionCount": 1, "providerCostUnavailableExecutionCount": 1,
			"actualPlatformAllocationCount": 1, "estimatedPlatformAllocationCount": 1,
		},
		"costs": map[string]any{
			"providerByCurrency": map[string]any{"USD": 15000},
			"platformByCurrency": map[string]any{"USD": 9000, "CNY": 3200},
			"knownByCurrency":    map[string]any{"USD": 24000, "CNY": 3200},
		},
		"controls": map[string]any{
			"tokenTotalsReconciled": true, "providerCoverageComplete": true,
			"actualOverridesEstimate": true, "currencySafeAggregation": true,
			"tenantIsolationValidated": true, "noPaymentDataPresent": true,
		},
		"manifest": map[string]any{"path": "internal-cost-manifest.json", "sha256": digest("internal-cost-manifest")},
		"evidence": evidence, "evidenceFileCount": 4,
		"cryptographicSignaturesVerified":                false,
		"externalSourceAndAuthorityVerificationRequired": true,
		"eligibleForHumanGateReview":                     true,
		"validatedAt":                                    "2026-07-31T04:00:00Z",
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
