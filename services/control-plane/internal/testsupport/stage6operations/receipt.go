package stage6operations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var operationIDs = []string{
	"platform.tenant-overview", "platform.tenant-provision", "platform.tenant-entitlement",
	"platform.release-candidate-lifecycle", "platform.release-role-decision",
	"platform.compliance-program-lifecycle", "platform.compliance-evidence-review", "platform.compliance-start-gate-decision",
	"platform.provider-commercial-lifecycle", "platform.provider-commercial-approval",
	"platform.governance-authority-lifecycle", "platform.incident-lifecycle",
	"platform.incident-public-communication", "platform.incident-security-resolution",
	"platform.slo-window-import", "platform.slo-window-decision", "platform.recovery-drill-import",
	"platform.recovery-drill-decision", "platform.penetration-engagement-import",
	"platform.penetration-engagement-decision", "platform.capacity-run-import", "platform.capacity-run-decision",
	"platform.incident-exercise-import", "platform.incident-exercise-decision", "user.tenant-self-service",
	"platform.internal-cost-review-import", "platform.internal-cost-review-decision",
	"platform.support-request", "platform.support-approve-revoke", "support.enter-readonly",
	"tenant.lifecycle-transition", "tenant.deletion-request-recovery", "tenant.member-governance",
	"tenant.identity-governance", "tenant.service-account-governance", "tenant.credential-governance",
	"support.credential-read", "tenant.worker-governance", "tenant.worker-release", "tenant.outbox-read",
	"tenant.outbox-replay", "tenant.audit-search-export", "tenant.legal-hold", "tenant.privacy-request",
	"tenant.data-export", "tenant.data-residency", "tenant.usage-quota", "tenant.support-policy",
}

type operationPolicy struct {
	surface, actor, negativeActor, access string
}

var operationPolicies = map[string]operationPolicy{
	"platform.tenant-overview":                 {"platform-admin", "platform-operator", "tenant-admin", "read"},
	"platform.tenant-provision":                {"platform-admin", "platform-admin", "platform-operator", "write"},
	"platform.tenant-entitlement":              {"platform-admin", "platform-admin", "platform-operator", "write"},
	"platform.release-candidate-lifecycle":     {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.release-role-decision":           {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.compliance-program-lifecycle":    {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.compliance-evidence-review":      {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.compliance-start-gate-decision":  {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.provider-commercial-lifecycle":   {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.provider-commercial-approval":    {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.governance-authority-lifecycle":  {"platform-admin", "platform-owner", "platform-admin", "four-eyes"},
	"platform.incident-lifecycle":              {"platform-admin", "platform-operator", "tenant-admin", "write"},
	"platform.incident-public-communication":   {"platform-admin", "platform-operator", "tenant-admin", "write"},
	"platform.incident-security-resolution":    {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.slo-window-import":               {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.slo-window-decision":             {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.recovery-drill-import":           {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.recovery-drill-decision":         {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.penetration-engagement-import":   {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.penetration-engagement-decision": {"platform-admin", "security-admin", "platform-operator", "four-eyes"},
	"platform.capacity-run-import":             {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.capacity-run-decision":           {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
	"platform.incident-exercise-import":        {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.incident-exercise-decision":      {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
	"platform.internal-cost-review-import":     {"platform-admin", "platform-admin", "security-admin", "write"},
	"platform.internal-cost-review-decision":   {"platform-admin", "platform-operator", "tenant-admin", "four-eyes"},
	"user.tenant-self-service":                 {"tenant-web", "authenticated-user", "unauthenticated", "write"},
	"platform.support-request":                 {"platform-admin", "platform-operator", "tenant-admin", "write"},
	"platform.support-approve-revoke":          {"platform-admin", "platform-admin", "platform-operator", "four-eyes"},
	"support.enter-readonly":                   {"platform-admin", "support-engineer", "tenant-admin", "read-only"},
	"tenant.lifecycle-transition":              {"tenant-web", "tenant-owner", "tenant-admin", "write"},
	"tenant.deletion-request-recovery":         {"tenant-web", "tenant-owner", "tenant-admin", "write"},
	"tenant.member-governance":                 {"tenant-web", "tenant-admin", "auditor", "write"},
	"tenant.identity-governance":               {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.service-account-governance":        {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.credential-governance":             {"tenant-web", "security-admin", "tenant-member", "write"},
	"support.credential-read":                  {"tenant-web", "support-engineer", "tenant-member", "read-only"},
	"tenant.worker-governance":                 {"tenant-web", "tenant-admin", "auditor", "write"},
	"tenant.worker-release":                    {"tenant-web", "tenant-admin", "auditor", "write"},
	"tenant.outbox-read":                       {"tenant-web", "support-engineer", "tenant-member", "read-only"},
	"tenant.outbox-replay":                     {"tenant-web", "tenant-admin", "auditor", "write"},
	"tenant.audit-search-export":               {"tenant-web", "auditor", "tenant-member", "read"},
	"tenant.legal-hold":                        {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.privacy-request":                   {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.data-export":                       {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.data-residency":                    {"tenant-web", "security-admin", "tenant-member", "write"},
	"tenant.usage-quota":                       {"tenant-web", "cost-admin", "auditor", "write"},
	"tenant.support-policy":                    {"tenant-web", "tenant-admin", "auditor", "write"},
}

var accountRoles = []string{
	"authenticated-user", "platform-operator", "platform-admin", "platform-owner", "support-engineer",
	"tenant-owner", "tenant-admin", "security-admin", "cost-admin", "auditor", "tenant-member",
}

func Receipt(candidateID, environmentID string) ([]byte, string, error) {
	accounts := make([]map[string]any, 0, len(accountRoles))
	for _, role := range accountRoles {
		accounts = append(accounts, map[string]any{
			"role": role, "subjectReference": "actor/" + role,
			"authenticationMethod": "sso", "sessionFreshAt": "2026-07-31T09:00:00Z",
		})
	}
	operations := make([]map[string]any, 0, len(operationIDs))
	for index, id := range operationIDs {
		policy := operationPolicies[id]
		var negativeSubject any = "actor/" + policy.negativeActor
		if policy.negativeActor == "unauthenticated" {
			negativeSubject = nil
		}
		operations = append(operations, map[string]any{
			"id": id, "surface": policy.surface, "actor": policy.actor, "access": policy.access,
			"actorSubjectReference": "actor/" + policy.actor, "status": "passed",
			"startedAt": "2026-07-31T10:00:00Z", "completedAt": "2026-07-31T10:01:00Z",
			"requestIds":    []string{uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s/%s/%d", environmentID, id, index))).String()},
			"negativeActor": policy.negativeActor, "negativeSubjectReference": negativeSubject, "negativeResult": "denied",
			"usedCli": false, "usedDatabaseClient": false, "usedDeveloperTools": false,
			"positiveEvidence": map[string]any{"path": fmt.Sprintf("operations/%02d-positive.json", index), "sha256": digest(id + "/positive")},
			"negativeEvidence": map[string]any{"path": fmt.Sprintf("operations/%02d-negative.json", index), "sha256": digest(id + "/negative")},
		})
	}
	receipt := map[string]any{
		"schemaVersion": "synara.stage6-operations-browser-exercise-validation.v2",
		"candidate": map[string]any{
			"candidateId": candidateID, "sourceCommit": strings.Repeat("a", 40),
			"environment": "production-like", "environmentId": environmentID,
			"artifacts": map[string]any{
				"controlPlane": "sha256:" + strings.Repeat("1", 64),
				"web":          "sha256:" + strings.Repeat("4", 64),
				"admin":        "sha256:" + strings.Repeat("5", 64),
			},
			"webBaseUrl": "https://app.example.test", "adminBaseUrl": "https://admin.example.test",
		},
		"matrix": map[string]any{
			"path":   "docs/release-matrices/stage-6-operations-ui-v1.json",
			"sha256": "sha256:a88ca1c8ea8797844692839b58b23bec057316f10cd7a585a61b8710bb4c2693",
		},
		"window":   map[string]any{"startedAt": "2026-07-31T10:00:00Z", "completedAt": "2026-07-31T11:00:00Z"},
		"accounts": accounts, "operations": operations,
		"operationCounts":             map[string]any{"blocked": 0, "failed": 0, "passed": len(operationIDs)},
		"negativeAuthorizationCounts": map[string]any{"denied": len(operationIDs), "not-run": 0, "unexpectedly-allowed": 0},
		"fallbackCounts":              map[string]any{"cli": 0, "databaseClient": 0, "developerTools": 0},
		"supportAccess": map[string]any{
			"requesterSubjectReference": "actor/platform-operator", "approverSubjectReference": "actor/platform-admin",
			"supportSubjectReference": "actor/support-engineer", "fourEyesProved": true,
			"readOnlyWriteDenied": true, "revocationAuditAction": "support.access_revoked",
			"expiryAuditAction": "support.access_expired", "revocationAuditVisible": true,
			"expiryAuditVisible": true, "tenantAuditVisible": true,
			"evidence": map[string]any{"path": "support-access.json", "sha256": digest("support-access")},
		},
		"approvals": []map[string]any{
			{"role": "operations", "subjectReference": "approver/operations", "approved": true,
				"approvedAt": "2026-07-31T11:10:00Z", "evidence": map[string]any{"path": "operations-approval.json", "sha256": digest("operations-approval")}},
			{"role": "security", "subjectReference": "approver/security", "approved": true,
				"approvedAt": "2026-07-31T11:15:00Z", "evidence": map[string]any{"path": "security-approval.json", "sha256": digest("security-approval")}},
		},
		"evidenceFileCount": len(operationIDs)*2 + 3, "environmentEligible": true,
		"productionAuthenticationDeclared": true, "supportLifecycleComplete": true,
		"approvalsComplete": true, "eligibleForHumanGateReview": true,
		"assessment": "evidence-validated-not-operations-passed", "validatedAt": "2026-07-31T12:00:00Z",
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, "", err
	}
	return encoded, digestBytes(encoded), nil
}

// LegacyV1Receipt is restricted to migration tests that seed the exact
// Operations receipt shape accepted before Migration 000162. New candidates
// and exercises must use Receipt.
func LegacyV1Receipt(candidateID, environmentID string) ([]byte, string, error) {
	encoded, _, err := Receipt(candidateID, environmentID)
	if err != nil {
		return nil, "", err
	}
	var receipt map[string]any
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return nil, "", err
	}
	receipt["schemaVersion"] = "synara.stage6-operations-browser-exercise-validation.v1"
	support := receipt["supportAccess"].(map[string]any)
	delete(support, "revocationAuditAction")
	delete(support, "expiryAuditAction")
	delete(support, "revocationAuditVisible")
	delete(support, "expiryAuditVisible")
	support["revokedOrExpired"] = true
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
