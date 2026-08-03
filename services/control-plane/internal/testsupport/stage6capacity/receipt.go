package stage6capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

func Receipt(environmentID string) ([]byte, string, error) {
	evidence := map[string]any{}
	for _, name := range []string{"workloadGeneratorEvidence", "prometheusRangeEvidence", "externalCanaryEvidence", "databaseEvidence", "kubernetesEvidence", "applicationLogEvidence", "resultSummaryEvidence"} {
		evidence[name] = map[string]any{"path": name + ".json", "sha256": digest(name)}
	}
	measurements := map[string]any{
		"availabilityGoodRatio": 1.0, "apiLatencyGoodRatio": 0.995,
		"executionStartGoodRatio": 0.995, "eventDelayGoodRatio": 1.0,
		"httpRequestCount": 10000, "executionStartCount": 100, "eventAppendCount": 1000,
		"maximumDatabaseConnectionUtilizationRatio": 0.7,
		"maximumControlPlaneCPUUtilizationRatio":    0.7,
		"maximumControlPlaneMemoryUtilizationRatio": 0.7,
		"maximumOutboxOldestSeconds":                30.0, "maximumQueueOldestSeconds": 20.0,
		"maximumWarmDeficitUnits": 0, "deadLetterCount": 0, "oomKillCount": 0,
		"unexpectedRestartCount": 0, "minimumTenantSuccessRatio": 0.9,
		"maximumTenantSuccessRatio": 1.0, "failedAssertionCount": 0,
		"tenantFairnessWithinObjective": true,
	}
	receipt := map[string]any{
		"schemaVersion": "synara.capacity-soak-evidence-receipt.v1",
		"runId":         uuid.NewSHA1(uuid.NameSpaceURL, []byte("synara-stage6-capacity/"+environmentID)).String(),
		"releaseCommit": strings.Repeat("a", 40), "environmentClass": "production-like", "environmentId": environmentID,
		"startedAt": "2026-07-29T00:00:00Z", "completedAt": "2026-07-30T00:00:00Z",
		"durationSeconds": 86400.0, "minimumDurationSeconds": 86400.0,
		"sampleIntervalSeconds": 30, "externalProbeRegions": 3, "externalProbeCoverageRatio": 1.0,
		"forecast":                map[string]any{"peakConcurrentSessions": 100.0, "peakExecutionStartsPerMinute": 50.0, "peakEventAppendsPerSecond": 100.0, "peakSSEConnections": 200.0, "requiredHeadroomPercent": 20.0},
		"load":                    map[string]any{"peakConcurrentSessions": 120.0, "peakExecutionStartsPerMinute": 60.0, "peakEventAppendsPerSecond": 120.0, "peakSSEConnections": 240.0},
		"forecastHeadroomCovered": true,
		"phases": []any{
			map[string]any{"name": "steady-peak", "startedAt": "2026-07-29T00:00:00Z", "completedAt": "2026-07-29T04:00:00Z", "durationSeconds": 14400.0, "loadMultiplier": 1.2},
			map[string]any{"name": "burst", "startedAt": "2026-07-29T04:00:00Z", "completedAt": "2026-07-29T08:00:00Z", "durationSeconds": 14400.0, "loadMultiplier": 2.0},
			map[string]any{"name": "tenant-hotspot", "startedAt": "2026-07-29T08:00:00Z", "completedAt": "2026-07-29T12:00:00Z", "durationSeconds": 14400.0, "loadMultiplier": 1.2},
			map[string]any{"name": "rolling-disruption", "startedAt": "2026-07-29T12:00:00Z", "completedAt": "2026-07-29T16:00:00Z", "durationSeconds": 14400.0, "loadMultiplier": 1.2},
			map[string]any{"name": "cooldown", "startedAt": "2026-07-29T16:00:00Z", "completedAt": "2026-07-30T00:00:00Z", "durationSeconds": 28800.0, "loadMultiplier": 0.5},
		},
		"measurements": measurements, "declaredMeasurementsWithinObjectives": true,
		"exercises": map[string]any{"externalProbeRegions": 3, "controlPlaneRollingRestart": true, "workerChurn": true, "databaseConnectionPressure": true, "outboxBackpressure": true, "tenantHotspot": true},
		"manifest":  map[string]any{"path": "capacity-manifest.json", "sha256": digest("capacity-manifest")},
		"evidence":  evidence, "releaseEligibleEnvironment": true, "eligibleForHumanGateReview": true,
		"validatedAt": "2026-07-30T01:00:00Z", "assessment": "evidence-validated-not-capacity-passed",
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
