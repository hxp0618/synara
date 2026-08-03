import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { AdminApp, adminSessionQueryKey } from "../App";
import {
  adminSession,
  desktopAccess,
  entitlementSnapshot,
  pendingGrant,
  platformOverview,
  platformTenant,
} from "../testFixtures";
import { PlatformDesktopAccess } from "./PlatformDesktopAccess";
import { PlatformAdminShell } from "./PlatformAdminShell";
import { PlatformComplianceGovernance } from "./PlatformComplianceGovernance";
import { PlatformReleaseGovernance } from "./PlatformReleaseGovernance";
import { PlatformProviderCommercialGovernance } from "./PlatformProviderCommercialGovernance";
import { PlatformGovernanceAuthorities } from "./PlatformGovernanceAuthorities";
import { PlatformIncidentGovernance } from "./PlatformIncidentGovernance";
import { PlatformSLOGovernance } from "./PlatformSLOGovernance";
import { PlatformRecoveryGovernance } from "./PlatformRecoveryGovernance";
import { PlatformPenetrationGovernance } from "./PlatformPenetrationGovernance";
import { PlatformCapacityGovernance } from "./PlatformCapacityGovernance";
import { PlatformIncidentExerciseGovernance } from "./PlatformIncidentExerciseGovernance";
import { PlatformOperationsExerciseGovernance } from "./PlatformOperationsExerciseGovernance";
import { PlatformSupportAccess } from "./PlatformSupportAccess";
import { platformQueryKeys } from "./platformQueries";

function createClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(platformQueryKeys.supportAccess, { items: [pendingGrant] });
  client.setQueryData(platformQueryKeys.entitlements(platformTenant.id), entitlementSnapshot);
  client.setQueryData(platformQueryKeys.desktopAccess(platformTenant.id), desktopAccess);
  client.setQueryData(platformQueryKeys.profile, {
    internalStatusBoard: { configured: true, url: "https://status.synara.example" },
  });
  client.setQueryData(platformQueryKeys.releaseCandidates, {
    items: [
      {
        id: "release-record-1",
        candidateId: "v0.6.3-stage6.1",
        sourceCommit: "a".repeat(40),
        lockfileSha256: `sha256:${"b".repeat(64)}`,
        evidenceBundleSha256: `sha256:${"c".repeat(64)}`,
        evidenceBundleSchema: "synara.stage6-candidate-evidence-bundle-validation.v3",
        evidenceBundleAssessment: "evidence-consistent-not-ga-approved",
        evidenceBundleValidatedAt: "2026-08-01T00:00:00Z",
        evidenceBundleReceiptSizeBytes: 4096,
        desktopArtifactSetSha256: `sha256:${"e".repeat(64)}`,
        evidenceReceiptBound: true,
        finalAssetSetSha256: `sha256:${"d".repeat(64)}`,
        environmentId: "stage6/production-like",
        impactDomains: ["code_change"],
        providerCommercialAuthorizations: [],
        privacyLegalRequired: false,
        requiredApprovalRoles: ["engineering", "operations", "security", "product"],
        state: "ready_for_review",
        version: 2,
        createdBy: "release-manager",
        decisionSummary: null,
        residualRiskDisposition: null,
        residualRisks: [],
        approvals: [
          {
            id: "release-approval-security",
            role: "security",
            decision: "approved",
            approverUserId: "release-security",
            approverEmail: "release-security@example.test",
            approverName: "Release Security",
            reason: "Security reviewed the exact immutable candidate evidence.",
            evidenceReference: "https://evidence.example.test/release/security",
            evidenceSha256:
              "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
            createdAt: "2026-08-01T00:01:00Z",
          },
        ],
        finalReview: null,
        approvedAt: null,
        releasedAt: null,
        rejectedAt: null,
        rolledBackAt: null,
        createdAt: "2026-08-01T00:00:00Z",
        updatedAt: "2026-08-01T00:01:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.releaseReadiness("release-record-1"), {
    candidateRecordId: "release-record-1",
    candidateId: "v0.6.3-stage6.1",
    candidateState: "ready_for_review",
    internalGatesSatisfied: false,
    approvalTransitionEligible: false,
    finalReviewGate: {
      id: "final_review",
      label: "Final Review release binding",
      satisfied: false,
      internalAssessment: "eligible-exact-final-review-bound",
      externalVerificationRequired: true,
      externalBoundary:
        "Receipt eligibility does not authenticate external evidence, signatures, corporate authority or GA approval.",
    },
    releaseTransitionEligible: false,
    externalGaStatus: "required_not_verified_by_synara",
    gates: [
      {
        id: "candidate_evidence",
        label: "Candidate evidence binding",
        satisfied: true,
        internalAssessment: "exact-candidate-evidence-bound",
        externalVerificationRequired: true,
        externalBoundary:
          "Receipt consistency does not authenticate any underlying control, deployment, signer or reviewer.",
      },
      {
        id: "provider_commercial_authorizations",
        label: "Provider commercial authorization bindings",
        satisfied: true,
        internalAssessment: "not-applicable-no-provider-commercial-impact",
        externalVerificationRequired: true,
        externalBoundary:
          "The binding does not verify signatures, legal interpretation or corporate authority.",
      },
      {
        id: "internal_cost",
        label: "Internal usage and cost",
        satisfied: false,
        internalAssessment: "approved-eligible-internal-cost-review",
        externalVerificationRequired: true,
        externalBoundary:
          "Usage, Provider cost and platform allocation source authority require external verification.",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.capacityRuns, { items: [] });
  client.setQueryData(platformQueryKeys.incidentExercises, { items: [] });
  client.setQueryData(platformQueryKeys.operationsExercises, { items: [] });
  client.setQueryData(
    ["control-plane", "platform", "incident-operators", adminSession.user.activeTenantId],
    {
      items: [
        {
          tenantId: adminSession.user.activeTenantId,
          userId: adminSession.user.userId,
          email: adminSession.user.email,
          displayName: adminSession.user.displayName,
          role: "owner",
          status: "active",
          joinedAt: "2026-08-01T00:00:00Z",
          createdAt: "2026-08-01T00:00:00Z",
          updatedAt: "2026-08-01T00:00:00Z",
        },
        {
          tenantId: adminSession.user.activeTenantId,
          userId: "incident-comms",
          email: "incident-comms@example.test",
          displayName: "Incident Comms",
          role: "admin",
          status: "active",
          joinedAt: "2026-08-01T00:00:00Z",
          createdAt: "2026-08-01T00:00:00Z",
          updatedAt: "2026-08-01T00:00:00Z",
        },
      ],
    },
  );
  client.setQueryData(platformQueryKeys.incidents, {
    items: [
      {
        id: "incident-record-1",
        incidentKey: "INC-2026-001",
        severity: "SEV-1",
        state: "identified",
        title: "Execution scheduling degradation",
        internalImpactSummary:
          "Users may observe delayed execution starts while the affected lane recovers.",
        broadInternalImpact: true,
        securityPrivacyImpact: false,
        internalStatusBoardOrigin: "https://status.synara.example",
        internalStatusBoardIncidentReference: "status-provider-incident-1",
        affectedComponents: ["execution-scheduling"],
        affectedRegions: ["global"],
        incidentCommander: {
          userId: adminSession.user.userId,
          email: adminSession.user.email,
          displayName: adminSession.user.displayName,
        },
        communicationsLead: {
          userId: "incident-comms",
          email: "incident-comms@example.test",
          displayName: "Incident Comms",
        },
        securityPrivacyLead: null,
        startedAt: "2026-08-01T00:00:00Z",
        impactConfirmedAt: "2026-08-01T00:01:00Z",
        cadence: {
          status: "on-time",
          firstInternalUpdateTargetAt: "2026-08-01T00:16:00Z",
          nextInternalUpdateTargetAt: "2026-08-01T00:35:00Z",
          firstInternalUpdateWithinTarget: true,
          overdueSeconds: 0,
        },
        internalUpdates: [
          {
            id: "incident-update-1",
            kind: "initial",
            summary: "We are investigating delayed execution starts.",
            publishedAt: "2026-08-01T00:05:00Z",
            evidenceReference:
              "https://status.synara.example/incidents/status-provider-incident-1/updates/initial",
            createdBy: {
              userId: "incident-comms",
              email: "incident-comms@example.test",
              displayName: "Incident Comms",
            },
            createdAt: "2026-08-01T00:05:00Z",
          },
        ],
        internalNotifications: [
          {
            id: "outbox-message-1",
            kind: "initial",
            status: "published",
            attempts: 0,
            availableAt: "2026-08-01T00:05:00Z",
            publishedAt: "2026-08-01T00:05:02Z",
            deadLetteredAt: null,
          },
        ],
        resolutionApproval: null,
        resolutionApprovals: [],
        resolvedAt: null,
        cancelledAt: null,
        version: 4,
        createdBy: adminSession.user.userId,
        createdAt: "2026-08-01T00:01:00Z",
        updatedAt: "2026-08-01T00:05:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.sloWindows, {
    items: [
      {
        id: "slo-window-record-1",
        candidateRecordId: "release-record-1",
        candidateId: "v0.6.3-stage6.1",
        windowId: "00000000-0000-4000-8000-000000000118",
        receiptSha256: `sha256:${"f".repeat(64)}`,
        receiptSizeBytes: 8192,
        releaseCommit: "a".repeat(40),
        environmentClass: "production-like",
        environmentId: "stage6/production-like",
        publicOrigin: "https://slo.synara.example",
        windowStartedAt: "2026-07-01T00:00:00Z",
        windowCompletedAt: "2026-07-31T00:00:00Z",
        validatedAt: "2026-07-31T01:00:00Z",
        queryRevision: "c".repeat(40),
        allObjectivesAssessable: true,
        allObjectivesMet: true,
        eligibleForHumanGateReview: true,
        worstBudgetRemainingRatio: 0.75,
        budgetPolicyState: "normal-delivery",
        state: "recorded",
        version: 1,
        createdBy: adminSession.user.userId,
        objectives: [
          {
            key: "availability",
            targetRatio: 0.999,
            goodRatio: 0.99975,
            sampleCount: 259200,
            errorBudgetRemainingRatio: 0.75,
            policyState: "normal-delivery",
            assessable: true,
            objectiveMet: true,
          },
        ],
        approvals: [],
        approvedAt: null,
        rejectedAt: null,
        createdAt: "2026-07-31T01:00:00Z",
        updatedAt: "2026-07-31T01:00:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.recoveryDrills, {
    items: [
      {
        id: "recovery-record-1",
        candidateRecordId: "release-record-1",
        candidateId: "v0.6.3-stage6.1",
        drillId: "4dc18f4e-7d97-40e9-85f2-38f313ebf246",
        receiptSha256: `sha256:${"a".repeat(64)}`,
        receiptSizeBytes: 4096,
        candidateBindingSha256: `sha256:${"b".repeat(64)}`,
        recoverySubjectSha256: `sha256:${"c".repeat(64)}`,
        startedAt: "2026-07-30T00:00:00Z",
        completedAt: "2026-07-30T02:00:00Z",
        validatedAt: "2026-07-30T03:00:00Z",
        measurementsWithinObjectives: true,
        allRestoreCanariesPassed: true,
        allSourceApprovalsApproved: true,
        eligibleForHumanGateReview: true,
        cryptographicSignaturesVerified: false,
        externalAuthorityVerificationRequired: true,
        state: "recorded",
        version: 1,
        createdBy: "release-creator",
        components: [
          {
            key: "postgresql",
            profile: "postgresql-pitr",
            sourceRegion: "region-one",
            restoreRegion: "region-two",
            measuredRpoSeconds: 120,
            rpoObjectiveSeconds: 300,
            measuredRtoSeconds: 1800,
            rtoObjectiveSeconds: 3600,
            rpoWithinObjective: true,
            rtoWithinObjective: true,
            restoreServedCanary: true,
          },
        ],
        approvals: [],
        approvedAt: null,
        rejectedAt: null,
        createdAt: "2026-07-30T03:00:00Z",
        updatedAt: "2026-07-30T03:00:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.penetrationEngagements, {
    items: [
      {
        id: "penetration-record-1",
        candidateRecordId: "release-record-1",
        candidateId: "v0.6.3-stage6.1",
        engagementId: "1dbad749-d05f-43fa-9971-766478435c98",
        receiptSha256: `sha256:${"d".repeat(64)}`,
        receiptSizeBytes: 8192,
        releaseCommit: "a".repeat(40),
        environmentClass: "production-like",
        environmentId: "stage6/security",
        deploymentProfile: "self-managed-kubernetes",
        startedAt: "2026-07-30T00:00:00Z",
        completedAt: "2026-07-30T03:00:00Z",
        reportIssuedAt: "2026-07-30T04:00:00Z",
        validatedAt: "2026-07-30T05:00:00Z",
        thirdPartyIndependenceDeclared: true,
        stage5DependencySatisfied: true,
        assetCoverageComplete: true,
        scopeCoverageComplete: true,
        methodologyCoverageComplete: true,
        noUnacceptedHighOrCriticalFindings: true,
        eligibleForHumanGateReview: true,
        cryptographicSignaturesVerified: false,
        externalAuthorityVerificationRequired: true,
        state: "recorded",
        version: 1,
        createdBy: "release-creator",
        assets: [
          {
            assetType: "control-plane-api",
            artifactSha256: `sha256:${"1".repeat(64)}`,
            tested: true,
          },
        ],
        approvals: [],
        approvedAt: null,
        rejectedAt: null,
        createdAt: "2026-07-30T05:00:00Z",
        updatedAt: "2026-07-30T05:00:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.compliancePrograms, {
    items: [
      {
        id: "compliance-program-1",
        programKey: "soc2-2026",
        framework: "soc2_type2",
        scopeVersion: "2026.1",
        scopeSummary: "Synara internal self-hosted services and production operations.",
        executiveSponsorUserId: "sponsor-user",
        auditorOrganization: "Independent Auditor LLP",
        auditorEngagementReference: "https://evidence.example.test/auditor",
        observationStart: "2026-08-01T00:00:00Z",
        observationEnd: "2027-02-01T00:00:00Z",
        evidenceRepositoryReference: "https://evidence.example.test/repository",
        evidenceAccessPolicyReference: "https://evidence.example.test/access-policy",
        evidenceRetentionDays: 365,
        vendorRegisterReference: "https://evidence.example.test/vendors",
        riskRegisterReference: "https://evidence.example.test/risks",
        state: "draft",
        version: 1,
        createdBy: "program-creator",
        recordCompletedAt: null,
        controls: [
          {
            id: "control-1",
            controlId: "CC6.1",
            family: "logical_access",
            title: "Logical access review",
            description: "Quarterly review of privileged access assignments.",
            ownerUserId: "security-owner",
            cadence: "quarterly",
            evidenceRequirement: "Retain an immutable review population and decision evidence.",
            createdBy: "program-creator",
            evidence: [
              {
                id: "evidence-1",
                evidenceId: "access-review-2026-q3",
                evidenceType: "access_review",
                periodStart: "2026-07-01T00:00:00Z",
                periodEnd: "2026-07-31T00:00:00Z",
                sourceReference: "https://evidence.example.test/access-review",
                sha256: "a".repeat(64),
                mediaType: "application/json",
                classification: "restricted",
                collectedAt: "2026-08-01T00:00:00Z",
                retentionUntil: "2027-08-01T00:00:00Z",
                submittedBy: "submitter-user",
                review: null,
                createdAt: "2026-08-01T00:00:00Z",
              },
            ],
            createdAt: "2026-08-01T00:00:00Z",
          },
        ],
        decisions: [],
        readiness: {
          assessment: "record-incomplete-not-audit-active",
          missingControlFamilies: [
            "change_release",
            "operations",
            "data_governance",
            "resilience",
            "vendor_provider",
            "security_testing",
          ],
          missingDecisionRoles: ["security", "operations", "legal_privacy", "executive"],
          hasAcceptedReleaseManifest: false,
          eligibleForRecordCompleteReview: false,
        },
        createdAt: "2026-08-01T00:00:00Z",
        updatedAt: "2026-08-01T00:00:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.providerCommercialAuthorizations, {
    items: [
      {
        id: "provider-authorization-1",
        authorizationKey: "openai-api-2026",
        provider: "codex",
        providerProduct: "OpenAI API",
        accountType: "Enterprise API organization",
        contractingEntity: "OpenAI contracting entity",
        credentialMode: "customer_byok",
        allowedCredentialScopes: ["organization", "tenant"],
        allowedRegions: ["us-east-1"],
        dataUsePolicy: "no_training",
        retentionPolicy: "Approved enterprise API retention policy.",
        termsEffectiveAt: "2026-08-01T00:00:00Z",
        termsReference: "https://evidence.example.test/terms",
        termsSha256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        agreementReference: "https://evidence.example.test/agreement",
        agreementSha256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        dpaReference: "https://evidence.example.test/dpa",
        dpaSha256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        prohibitedUseSummary: "Consumer login sharing and safety-control bypass are prohibited.",
        terminationRunbookReference: "https://evidence.example.test/termination",
        terminationRunbookSha256:
          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        reviewExpiresAt: "2026-11-01T00:00:00Z",
        state: "ready_for_review",
        version: 2,
        createdBy: "provider-commercial-manager",
        approvals: [
          {
            id: "provider-approval-security",
            role: "security",
            decision: "approved",
            approverUserId: "security-reviewer",
            reason: "Security approved the exact hosted Provider boundary.",
            evidenceReference: "https://evidence.example.test/provider/security",
            evidenceSha256:
              "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
            createdAt: "2026-08-01T00:00:30Z",
          },
        ],
        activatedAt: null,
        rejectedAt: null,
        revokedAt: null,
        createdAt: "2026-08-01T00:00:00Z",
        updatedAt: "2026-08-01T00:01:00Z",
      },
    ],
  });
  client.setQueryData(platformQueryKeys.governanceAuthorities, {
    authorityKeys: ["release.engineering", "provider_commercial.security"],
    operators: [
      {
        userId: "release-engineer",
        email: "release-engineer@example.test",
        displayName: "Release Engineer",
        role: "admin",
      },
    ],
    items: [
      {
        id: "authority-1",
        userId: "release-engineer",
        userEmail: "release-engineer@example.test",
        userDisplayName: "Release Engineer",
        authorityKey: "release.engineering",
        status: "active",
        version: 1,
        expiresAt: "2026-12-01T00:00:00Z",
        grantedBy: adminSession.user.userId,
        reason: "Owner verified the Engineering release function.",
        evidenceReference: "https://evidence.example.test/authority/engineering",
        evidenceSha256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        revokedAt: null,
        revokedBy: null,
        revocationReason: null,
        createdAt: "2026-08-01T00:00:00Z",
        updatedAt: "2026-08-01T00:00:00Z",
      },
    ],
  });
  return client;
}

function renderWithClient(client: QueryClient, children: ReactNode): string {
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  );
}

describe("Platform Admin authority boundary", () => {
  it("keeps authority and customer-context indicators persistent for a Platform Owner", () => {
    const client = createClient();
    const markup = renderWithClient(
      client,
      <PlatformAdminShell session={adminSession} overview={platformOverview("owner")} />,
    );

    expect(markup).toContain("Platform authority");
    expect(markup).toContain("Tenant context:");
    expect(markup).toContain("Provision Tenant");
    expect(markup).toContain("Entitlements");
    expect(markup).toContain("Desktop access");
    expect(markup).toContain("Release governance");
    expect(markup).toContain("Compliance");
    expect(markup).toContain("Provider use");
    expect(markup).toContain("Governance roles");
    expect(markup).toContain("Internal status");
    expect(markup).toContain('href="https://status.synara.example"');
    expect(markup).toContain("Northstar Labs");
  });

  it("keeps functional authority owner-managed and visible to reviewers", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformGovernanceAuthorities canManage={false} currentUserId="reviewer" />,
    );
    const ownerMarkup = renderWithClient(
      client,
      <PlatformGovernanceAuthorities canManage currentUserId={adminSession.user.userId} />,
    );

    expect(reviewerMarkup).toContain("Release Engineer");
    expect(reviewerMarkup).toContain(
      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    );
    expect(reviewerMarkup).toContain("Only an Operator Tenant Owner");
    expect(reviewerMarkup).not.toContain("Assign exact functional authority");
    expect(ownerMarkup).toContain("Assign exact functional authority");
    expect(ownerMarkup).toContain("Revoke authority");
  });

  it("separates Provider commercial review from authorization management", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformProviderCommercialGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(
      client,
      <PlatformProviderCommercialGovernance canManage />,
    );

    expect(reviewerMarkup).toContain("openai-api-2026");
    expect(reviewerMarkup).toContain(
      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    );
    expect(reviewerMarkup).toContain(
      "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    );
    expect(reviewerMarkup).toContain("Record separated commercial decision");
    expect(reviewerMarkup).not.toContain("Create immutable commercial authorization");
    expect(reviewerMarkup).not.toContain("Advance or revoke authorization");
    expect(managerMarkup).toContain("Create immutable commercial authorization");
    expect(managerMarkup).toContain("Advance or revoke authorization");
  });

  it("separates compliance evidence review from program management", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformComplianceGovernance canManage={false} currentUserId={adminSession.user.userId} />,
    );
    const managerMarkup = renderWithClient(
      client,
      <PlatformComplianceGovernance canManage currentUserId={adminSession.user.userId} />,
    );

    expect(reviewerMarkup).toContain("soc2-2026");
    expect(reviewerMarkup).toContain("Independently review evidence");
    expect(reviewerMarkup).toContain("Exact review evidence SHA-256");
    expect(reviewerMarkup).not.toContain("Create immutable program scope");
    expect(reviewerMarkup).not.toContain("Add append-only control");
    expect(reviewerMarkup).not.toContain("Advance compliance record");
    expect(managerMarkup).toContain("Create immutable program scope");
    expect(managerMarkup).toContain("Add append-only control");
    expect(managerMarkup).toContain("Advance compliance record");
    expect(managerMarkup).toContain("Exact review evidence SHA-256");
  });

  it("separates release review from candidate-management authority", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformReleaseGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(client, <PlatformReleaseGovernance canManage />);

    expect(reviewerMarkup).toContain("v0.6.3-stage6.1");
    expect(reviewerMarkup).toContain("Record separated decision");
    expect(reviewerMarkup).toContain("Exact-candidate readiness");
    expect(reviewerMarkup).toContain("Candidate evidence binding · satisfied internally");
    expect(reviewerMarkup).toContain(
      "Provider commercial authorization bindings · satisfied internally",
    );
    expect(reviewerMarkup).toContain("Internal usage and cost · missing");
    expect(reviewerMarkup).toContain("External GA status · not verified by Synara");
    expect(reviewerMarkup).toContain("Final Review release binding · missing");
    expect(reviewerMarkup).toContain("Released transition: not eligible");
    expect(reviewerMarkup).toContain(
      "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
    );
    expect(reviewerMarkup).toContain("Unbound — cannot enter released");
    expect(reviewerMarkup).not.toContain("Create candidate");
    expect(reviewerMarkup).not.toContain("Advance candidate state");
    expect(managerMarkup).toContain("Create candidate");
    expect(managerMarkup).toContain("Advance candidate state");
  });

  it("uses the immutable Final Review decision as the released transition source", () => {
    const client = createClient();
    const current = client.getQueryData(platformQueryKeys.releaseCandidates) as {
      items: Array<Record<string, unknown>>;
    };
    client.setQueryData(platformQueryKeys.releaseCandidates, {
      items: [
        {
          ...current.items[0],
          state: "observing",
          version: 6,
          finalReview: {
            id: "final-review-1",
            receiptSha256: `sha256:${"f".repeat(64)}`,
            schemaVersion: "synara.stage6-final-ga-review-validation.v1",
            assessment: "final-review-consistent-not-ga-authority-verified",
            validatedAt: "2026-08-01T12:00:00Z",
            receiptSizeBytes: 8192,
            controlInventorySha256: `sha256:${"a".repeat(64)}`,
            controlCount: 32,
            finalApprovalCount: 4,
            decisionSummary:
              "Release the exact reviewed candidate after its bounded observation window.",
            residualRiskDisposition: "accepted",
            residualRisks: [
              {
                id: "risk.capacity-headroom",
                summary: "Capacity headroom remains below the preferred operating target.",
                owner: "Operations",
                dueAt: "2026-09-01T12:00:00Z",
                acceptanceReason:
                  "The bounded exposure is accepted for the initial rollout window.",
                evidenceReference: "https://evidence.example.test/risks/capacity-headroom",
              },
            ],
            allRequiredControlsPassed: true,
            allRequiredFinalApprovalsApproved: true,
            eligibleForExternalGAAuthorityReview: true,
            boundBy: "release-manager",
            createdAt: "2026-08-01T12:01:00Z",
          },
        },
      ],
    });

    const markup = renderWithClient(client, <PlatformReleaseGovernance canManage />);

    expect(markup).toContain("Immutable Final Review decision");
    expect(markup).toContain(
      "Release the exact reviewed candidate after its bounded observation window.",
    );
    expect(markup).toContain("risk.capacity-headroom");
    expect(markup).toContain("cannot be edited during the released transition");
    expect(markup).not.toContain("Add residual risk");
  });

  it("renders governed incident roles, internal evidence, cadence and commander-only transition", () => {
    const client = createClient();
    const markup = renderWithClient(client, <PlatformIncidentGovernance session={adminSession} />);

    expect(markup).toContain("Incident governance");
    expect(markup).toContain("Execution scheduling degradation");
    expect(markup).toContain("Incident Comms");
    expect(markup).toContain("Open immutable internal Status Board evidence");
    expect(markup).toContain("Employee notification intents");
    expect(markup).toContain("initial · published");
    expect(markup).toContain("not proof that the internal Status Board");
    expect(markup).toContain("Commander transition");
    expect(markup).not.toContain("Record internal incident update");
  });

  it("renders exact SLO budget projections and separated review without claiming production pass", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(client, <PlatformSLOGovernance canManage={false} />);
    const managerMarkup = renderWithClient(client, <PlatformSLOGovernance canManage />);

    expect(reviewerMarkup).toContain("SLO windows");
    expect(reviewerMarkup).toContain("v0.6.3-stage6.1");
    expect(reviewerMarkup).toContain("Worst remaining budget");
    expect(reviewerMarkup).toContain("Record separated SLO decision");
    expect(reviewerMarkup).toContain("Exact approval evidence SHA-256");
    expect(reviewerMarkup).not.toContain("Import exact SLO validator receipt");
    expect(managerMarkup).toContain("Import exact SLO validator receipt");
    expect(managerMarkup).toContain("not a production SLO claim");
  });

  it("renders candidate-bound Recovery projections and separated review without authenticating external backup authority", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformRecoveryGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(client, <PlatformRecoveryGovernance canManage />);

    expect(reviewerMarkup).toContain("Recovery drills");
    expect(reviewerMarkup).toContain("postgresql-pitr");
    expect(reviewerMarkup).toContain("Record separated Recovery decision");
    expect(reviewerMarkup).toContain("Exact approval evidence SHA-256");
    expect(reviewerMarkup).toContain("backup, approver and signature authority still required");
    expect(reviewerMarkup).not.toContain("Import exact Recovery v2 receipt");
    expect(managerMarkup).toContain("Import exact Recovery v2 receipt");
    expect(managerMarkup).toContain("does not authenticate an external backup authority");
  });

  it("renders candidate-bound Penetration projections without authenticating the external assessor", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformPenetrationGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(client, <PlatformPenetrationGovernance canManage />);

    expect(reviewerMarkup).toContain("Penetration reviews");
    expect(reviewerMarkup).toContain("control-plane-api");
    expect(reviewerMarkup).toContain("Record separated Penetration decision");
    expect(reviewerMarkup).toContain("Exact approval evidence SHA-256");
    expect(reviewerMarkup).toContain("assessor identity, report, signatures and execution");
    expect(reviewerMarkup).not.toContain("Import exact Penetration receipt");
    expect(managerMarkup).toContain("Import exact Penetration receipt");
    expect(managerMarkup).toContain("does not authenticate the assessor");
  });

  it("renders Capacity governance without authenticating external execution evidence", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformCapacityGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(client, <PlatformCapacityGovernance canManage />);

    expect(reviewerMarkup).toContain("Capacity reviews");
    expect(reviewerMarkup).toContain("No Capacity reviews");
    expect(reviewerMarkup).toContain("does not authenticate the environment");
    expect(reviewerMarkup).not.toContain("Import exact Capacity receipt");
    expect(managerMarkup).toContain("Import exact Capacity receipt");
  });

  it("renders Incident exercise governance without authenticating deployed delivery evidence", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformIncidentExerciseGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(
      client,
      <PlatformIncidentExerciseGovernance canManage />,
    );

    expect(reviewerMarkup).toContain("Incident exercises");
    expect(reviewerMarkup).toContain("No Incident exercise reviews");
    expect(reviewerMarkup).toContain("does not authenticate deployed delivery");
    expect(reviewerMarkup).not.toContain("Import exact Incident exercise receipt");
    expect(managerMarkup).toContain("Import exact Incident exercise receipt");
  });

  it("renders Operations exercise governance without authenticating deployed browser evidence", () => {
    const client = createClient();
    const reviewerMarkup = renderWithClient(
      client,
      <PlatformOperationsExerciseGovernance canManage={false} />,
    );
    const managerMarkup = renderWithClient(
      client,
      <PlatformOperationsExerciseGovernance canManage />,
    );

    expect(reviewerMarkup).toContain("Operations exercises");
    expect(reviewerMarkup).toContain("No Operations exercise reviews");
    expect(reviewerMarkup).toContain("does not authenticate the deployment");
    expect(reviewerMarkup).not.toContain("Import exact Operations exercise receipt");
    expect(managerMarkup).toContain("Import exact Operations exercise receipt");
  });

  it("does not mount Platform Admin writes for a security operator", () => {
    const client = createClient();
    const shellMarkup = renderWithClient(
      client,
      <PlatformAdminShell session={adminSession} overview={platformOverview("security_admin")} />,
    );
    const supportMarkup = renderWithClient(
      client,
      <PlatformSupportAccess
        session={adminSession}
        overview={platformOverview("security_admin")}
        onNavigate={() => undefined}
      />,
    );

    expect(shellMarkup).not.toContain("Provision Tenant");
    expect(shellMarkup).not.toContain(">Entitlements<");
    expect(shellMarkup).not.toContain("Desktop access");
    expect(supportMarkup).toContain("Request Support Access");
    expect(supportMarkup).not.toContain("Decide or revoke Support Access");
    expect(supportMarkup).not.toContain(">Approve<");
    expect(supportMarkup).not.toContain(">Deny<");
  });

  it("shows exact Desktop Enrollment context and blocks unverified cross-user issuance", () => {
    const client = createClient();
    const markup = renderWithClient(
      client,
      <PlatformDesktopAccess
        session={adminSession}
        overview={platformOverview("owner")}
        onNavigate={() => undefined}
      />,
    );

    expect(markup).toContain("Generate and open Synara Desktop");
    expect(markup).toContain("https://control.synara.example");
    expect(markup).toContain("3 minutes");
    expect(markup).toContain("Internal User · member · IdP required");
    expect(markup).toContain("Platform authority cannot mint their Desktop session");
  });

  it("cuts off all global workflows while support_readonly context is active", () => {
    const client = createClient();
    client.setQueryData(adminSessionQueryKey, {
      ...adminSession,
      user: {
        ...adminSession.user,
        activeTenantId: platformTenant.id,
        supportAccessGrantId: "grant-active",
      },
      tenants: [
        ...adminSession.tenants,
        {
          id: platformTenant.id,
          slug: platformTenant.slug,
          name: platformTenant.name,
          status: "active",
          entitlementProfileCode: platformTenant.entitlementProfileCode,
          region: platformTenant.region,
          role: "support_readonly",
        },
      ],
    });
    const markup = renderWithClient(client, <AdminApp />);

    expect(markup).toContain("support_readonly");
    expect(markup).toContain("Global Platform queries and writes are not mounted");
    expect(markup).not.toContain("Tenant operations");
    expect(markup).not.toContain("Provision Tenant");
    expect(markup).not.toContain("Decide or revoke Support Access");
  });
});
