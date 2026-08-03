package stage6recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

var approvalRoles = []string{"database", "kms", "operations", "security", "storage"}

var componentObjectives = map[string]map[string][2]float64{
	"postgresql":     {"postgresql-pitr": {300, 3600}},
	"object-storage": {"object-versioned-replica": {900, 7200}},
	"kms": {
		"kms-cloud-multi-region":  {0, 3600},
		"kms-vault-raft-snapshot": {86400, 7200},
	},
	"queue": {"queue-postgres-outbox-replay": {300, 3600}},
}

// Receipt creates an eligible exact Recovery v2 receipt for integration tests.
// It intentionally retains the external-authority boundary instead of claiming
// that fixture approvals or backup evidence were cryptographically verified.
func Receipt(candidate persistence.Stage6ReleaseCandidate, validatedAt time.Time) ([]byte, string, error) {
	validatedAt = validatedAt.UTC().Truncate(time.Second)
	var candidateReceipt struct {
		Candidate map[string]json.RawMessage `json:"candidate"`
	}
	if err := json.Unmarshal(candidate.EvidenceBundleReceipt, &candidateReceipt); err != nil || candidateReceipt.Candidate == nil {
		return nil, "", errors.New("candidate evidence receipt is invalid")
	}
	delete(candidateReceipt.Candidate, "desktopArtifactSetSha256")
	candidateJSON, err := json.Marshal(candidateReceipt.Candidate)
	if err != nil {
		return nil, "", err
	}
	var candidateValue map[string]any
	if err := json.Unmarshal(candidateJSON, &candidateValue); err != nil {
		return nil, "", err
	}
	regions, ok := candidateValue["regions"].([]any)
	if !ok || len(regions) < 2 {
		return nil, "", errors.New("Recovery receipt fixture requires at least two candidate regions")
	}
	sourceRegion, sourceOK := regions[0].(string)
	restoreRegion, restoreOK := regions[1].(string)
	if !sourceOK || !restoreOK || sourceRegion == "" || restoreRegion == "" || sourceRegion == restoreRegion {
		return nil, "", errors.New("Recovery receipt fixture requires two distinct named candidate regions")
	}
	restoredValue := map[string]any{
		"sourceCommit":   candidateValue["sourceCommit"],
		"lockfileSha256": candidateValue["lockfileSha256"],
		"artifacts":      candidateValue["artifacts"],
		"migrationTail":  candidateValue["migrationTail"],
	}
	completedAt := validatedAt.Add(-time.Hour)
	startedAt := completedAt.Add(-2 * time.Hour)
	profiles := map[string]string{
		"postgresql": "postgresql-pitr", "object-storage": "object-versioned-replica",
		"kms": "kms-vault-raft-snapshot", "queue": "queue-postgres-outbox-replay",
	}
	components := map[string]any{}
	for index, key := range []string{"kms", "object-storage", "postgresql", "queue"} {
		objectives := componentObjectives[key][profiles[key]]
		restoredThrough := startedAt.Add(time.Duration(5+index) * time.Minute)
		measuredRPO := math.Min(objectives[0], 120)
		sourceCutoff := restoredThrough.Add(time.Duration(measuredRPO) * time.Second)
		recoveryStarted := sourceCutoff.Add(5 * time.Minute)
		measuredRTO := math.Min(objectives[1], 1800)
		serviceReady := recoveryStarted.Add(time.Duration(measuredRTO) * time.Second)
		components[key] = map[string]any{
			"profile": profiles[key], "sourceAuthority": "source-" + key, "restoreTarget": "restore-" + key,
			"sourceRegion": sourceRegion, "restoreRegion": restoreRegion,
			"sourceFailureDomain": sourceRegion + "/source", "restoreFailureDomain": restoreRegion + "/restore",
			"sourceCutoffAt": formatUTC(sourceCutoff), "restoredThroughAt": formatUTC(restoredThrough),
			"recoveryStartedAt": formatUTC(recoveryStarted), "serviceReadyAt": formatUTC(serviceReady),
			"measuredRpoSeconds": sourceCutoff.Sub(restoredThrough).Seconds(), "rpoObjectiveSeconds": objectives[0], "rpoWithinObjective": true,
			"measuredRtoSeconds": serviceReady.Sub(recoveryStarted).Seconds(), "rtoObjectiveSeconds": objectives[1], "rtoWithinObjective": true,
			"restoreServedCanary": true,
			"evidence":            map[string]any{"backupEvidence": evidenceRef(key + "-backup"), "restoreEvidence": evidenceRef(key + "-restore"), "clientEvidence": evidenceRef(key + "-client")},
		}
	}
	drillID := uuid.New()
	candidateBinding := sha256.Sum256(mustJSON(candidateValue))
	subject := map[string]any{
		"drillId": drillID.String(), "candidate": candidateValue,
		"startedAt": formatUTC(startedAt), "completedAt": formatUTC(completedAt),
		"restoredReleaseIdentity": restoredValue, "components": components,
	}
	subjectDigest := sha256.Sum256(mustJSON(subject))
	subjectSHA := digest(subjectDigest[:])
	approvals := map[string]any{}
	approvalEvidence := map[string]any{}
	for _, role := range approvalRoles {
		ref := evidenceRef(role + "-approval")
		approvals[role] = map[string]any{
			"role": role, "approverId": role + "-source-approver", "decision": "approved-for-human-gate-review",
			"approvedAt": formatUTC(completedAt.Add(15 * time.Minute)), "expiresAt": formatUTC(validatedAt.Add(30 * 24 * time.Hour)),
			"subjectSha256": subjectSHA, "evidence": ref,
		}
		approvalEvidence[role] = ref
	}
	receipt := map[string]any{
		"schemaVersion": "synara.recovery-drill-evidence-receipt.v2", "drillId": drillID.String(),
		"candidate": candidateValue, "candidateBindingSha256": digest(candidateBinding[:]),
		"restoredReleaseIdentity": restoredValue, "startedAt": formatUTC(startedAt), "completedAt": formatUTC(completedAt),
		"recoverySubjectSha256": subjectSHA, "manifest": evidenceRef("manifest"), "components": components,
		"approvals": approvals, "approvalEvidence": approvalEvidence,
		"declaredMeasurementsWithinObjectives": true, "allRestoreCanariesPassed": true, "allRequiredApprovalsApproved": true,
		"releaseEligibleEnvironment": true, "eligibleForHumanGateReview": true,
		"verificationBoundary": map[string]any{
			"approvalContentAndSubjectValidated": true, "cryptographicSignaturesVerified": false,
			"realBackupRestoreAndApproverAuthorityVerificationRequired": true,
		},
		"validatedAt": formatUTC(validatedAt), "assessment": "evidence-validated-not-control-passed",
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, "", err
	}
	return encoded, digestBytes(encoded), nil
}

func evidenceRef(value string) map[string]any {
	sum := sha256.Sum256([]byte(value))
	return map[string]any{"path": value + ".json", "sha256": digest(sum[:])}
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return digest(sum[:])
}

func digest(value []byte) string { return "sha256:" + hex.EncodeToString(value) }
func mustJSON(value any) []byte  { encoded, _ := json.Marshal(value); return encoded }
func formatUTC(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
