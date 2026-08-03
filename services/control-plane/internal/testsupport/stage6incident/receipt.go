package stage6incident

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

func Receipt(environmentID string) ([]byte, string, error) {
	components := map[string]any{}
	for _, name := range []string{
		"control-plane-api", "authentication-sso", "execution-scheduling",
		"worker-runtime", "artifact-service", "web-application",
	} {
		components[name] = map[string]any{"published": true, "regionDetailPublished": false}
	}
	evidence := map[string]any{}
	for _, name := range []string{
		"onCallRotaEvidence", "pagingProviderEvidence", "internalStatusBoardTimelineEvidence",
		"employeeNotificationDeliveryEvidence", "internalProbeEvidence", "internalUserPathEvidence",
		"exerciseReviewEvidence",
	} {
		evidence[name] = map[string]any{"path": name + ".json", "sha256": digest(name)}
	}
	receipt := map[string]any{
		"schemaVersion":    "synara.incident-communication-exercise-evidence-receipt.v2",
		"exerciseId":       uuid.NewSHA1(uuid.NameSpaceURL, []byte(environmentID)).String(),
		"releaseCommit":    strings.Repeat("a", 40),
		"environmentClass": "production-like", "environmentId": environmentID,
		"exerciseMode": "live-internal-communication", "severity": "SEV-1",
		"serviceOrigin": "https://app.example.test", "internalStatusBoardOrigin": "https://status.example.test",
		"independentInternalStatusBoardDeclared": true, "regionPromiseEnabled": false,
		"startedAt": "2026-07-30T00:00:00Z", "completedAt": "2026-07-30T02:00:00Z",
		"manifest": map[string]any{"path": "manifest.json", "sha256": digest("manifest")},
		"roles": map[string]any{
			"incidentCommander": "actor/ic", "operationsLead": "actor/operations",
			"communicationsLead": "actor/comms", "scribe": "actor/scribe",
			"securityPrivacyLead": "actor/security", "releaseObserver": "actor/release",
		},
		"roleSeparationComplete": true,
		"paging": map[string]any{
			"providerReference": "paging/provider-1", "primaryResponder": "actor/primary",
			"secondaryResponder": "actor/secondary", "pageTriggeredAt": "2026-07-30T00:11:00Z",
			"acknowledgedAt": "2026-07-30T00:14:00Z", "acknowledgedBy": "actor/primary",
			"acknowledgementSeconds": 180, "targetSeconds": 300, "withinTarget": true,
			"escalationExercised": true, "pagingSucceeded": true,
		},
		"pagingExerciseComplete":        true,
		"internalStatusBoardComponents": components, "internalStatusBoardComponentsComplete": true,
		"internalTimeline": map[string]any{
			"impactConfirmedAt": "2026-07-30T00:10:00Z", "incidentOpenedAt": "2026-07-30T00:12:00Z",
			"updates": []map[string]any{
				{"kind": "initial", "publishedAt": "2026-07-30T00:20:00Z"},
				{"kind": "progress", "publishedAt": "2026-07-30T00:45:00Z"},
				{"kind": "progress", "publishedAt": "2026-07-30T01:10:00Z"},
				{"kind": "resolved", "publishedAt": "2026-07-30T01:40:00Z"},
			},
			"firstInternalUpdateSeconds": 600, "firstInternalUpdateTargetSeconds": 900,
			"firstInternalUpdateWithinTarget": true, "updateCadenceTargetSeconds": 1800,
			"updateCadenceWithinTarget": true, "internalHistoryVisible": true,
		},
		"internalTimelineWithinTargets": true,
		"employeeNotificationDelivery": map[string]any{
			"channelReference":              "employee/email-canary",
			"initialNotificationReceivedAt": "2026-07-30T00:21:00Z", "initialDeliverySeconds": 60,
			"resolutionNotificationReceivedAt": "2026-07-30T01:41:00Z", "resolutionDeliverySeconds": 60,
			"deliverySucceeded": true,
		},
		"employeeNotificationDeliveryComplete": true,
		"recoveryVerification": map[string]any{
			"recoveredAt": "2026-07-30T01:20:00Z", "observationSecondsBeforeResolution": 1200,
			"minimumObservationSeconds": 900, "internalProbePassed": true,
			"internalUserPathPassed": true, "cleanupCompleted": true,
		},
		"recoveryVerificationComplete": true,
		"review": map[string]any{
			"incidentTimelineConsistent": true, "correctiveActionsOwned": true,
			"sensitiveDataAbsent": true, "operationsApproved": true, "communicationsApproved": true,
		},
		"reviewComplete": true, "evidence": evidence,
		"releaseEligibleEnvironment": true, "eligibleForHumanGateReview": true,
		"validatedAt": "2026-07-30T03:00:00Z", "assessment": "evidence-validated-not-operations-ready",
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
