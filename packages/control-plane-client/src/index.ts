import type {
  OrchestrationLatestTurn,
  ProviderCapabilityMap,
  ProviderCapabilityProjection,
  ProviderHostProviderKind,
  ProviderInteractionMode,
  ProviderKind,
  ProviderReleasePolicy,
  ProviderRuntimeDescriptor,
  ProviderSupportTier,
  ProviderUserInputAnswers,
  RuntimeMode,
} from "@synara/contracts";

export type ControlPlaneHttpUrlResolver = (path: string) => string;

export type ControlPlaneClientTransportOptions = {
  /** Host-specific URL resolution, for example a desktop custom-protocol WebSocket bridge. */
  resolveHttpUrl?: ControlPlaneHttpUrlResolver;
};

let configuredHttpUrlResolver: ControlPlaneHttpUrlResolver | null = null;

/**
 * Installs the narrow host transport adapter used by the shared singleton. Browser hosts need no
 * configuration; desktop hosts can supply their existing authenticated HTTP bridge without making
 * this package depend on Electron or Web application internals.
 */
export function configureControlPlaneClientTransport(
  options: ControlPlaneClientTransportOptions = {},
): void {
  configuredHttpUrlResolver = options.resolveHttpUrl ?? null;
}

export function resolveControlPlaneHttpUrl(path: string): string {
  if (
    typeof window !== "undefined" &&
    (window.location.protocol === "http:" || window.location.protocol === "https:")
  ) {
    return new URL(path, window.location.origin).toString();
  }
  if (configuredHttpUrlResolver) {
    return configuredHttpUrlResolver(path);
  }
  return path;
}

export class ControlPlaneError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId?: string,
    readonly details?: Readonly<Record<string, unknown>>,
  ) {
    super(message);
    this.name = "ControlPlaneError";
  }
}

export type ControlPlaneTenantAccess = {
  id: string;
  slug: string;
  name: string;
  status: "evaluation" | "active" | "suspended" | "closed";
  lifecycleVersion?: number;
  evaluationExpiresAt?: string | null;
  suspendedAt?: string | null;
  closedAt?: string | null;
  entitlementProfileCode: string;
  region: string;
  role:
    | "owner"
    | "admin"
    | "security_admin"
    | "cost_admin"
    | "auditor"
    | "member"
    | "support_readonly";
};

export type ControlPlaneCreateTenantInput = {
  slug: string;
  name: string;
  region?: string;
};

export type ControlPlaneProvisionTenantInput = {
  ownerEmail: string;
  slug: string;
  name: string;
  region: string;
  entitlementProfileCode: "standard" | "enterprise";
  status: "active" | "evaluation";
  evaluationExpiresAt?: string;
};

export type ControlPlaneProvisionedTenant = {
  id: string;
  slug: string;
  name: string;
  status: "active" | "evaluation";
  lifecycleVersion: number;
  evaluationExpiresAt: string | null;
  entitlementProfileCode: string;
  region: string;
  ownerUserId: string;
  ownerEmail: string;
  createdAt: string;
};

export type ControlPlaneAssignTenantEntitlementProfileInput = {
  entitlementProfileCode: "standard" | "enterprise";
  status: "active" | "evaluation";
  expectedVersion: number;
  evaluationEndsAt?: string;
  reportingPeriodStart: string;
  reportingPeriodEnd: string;
  reason: string;
};

export type ControlPlaneDeletingTenant = Omit<
  ControlPlaneTenantAccess,
  "status" | "lifecycleVersion"
> & {
  status: "deleting";
  lifecycleVersion: number;
  deletionRequestedAt: string;
};

export type ControlPlaneResourceLifecycleWarmPoolMode = "disabled" | "balanced" | "low-latency";

export type ControlPlaneResourceLifecycleIntBounds = {
  min: number;
  max: number;
};

export type ControlPlaneResourceLifecycleBounds = {
  waitingKeepAliveSeconds: ControlPlaneResourceLifecycleIntBounds;
  suspendAfterIdleSeconds: ControlPlaneResourceLifecycleIntBounds;
  absoluteSessionLifetimeSeconds: ControlPlaneResourceLifecycleIntBounds;
  workspaceRetentionDays: ControlPlaneResourceLifecycleIntBounds;
  warmPoolModes: ReadonlyArray<ControlPlaneResourceLifecycleWarmPoolMode>;
};

export type ControlPlaneResourceLifecycleOverrides = {
  waitingKeepAliveSeconds: number | null;
  suspendAfterIdleSeconds: number | null;
  absoluteSessionLifetimeSeconds: number | null;
  workspaceRetentionDays: number | null;
  warmPoolMode: ControlPlaneResourceLifecycleWarmPoolMode | null;
};

export type ControlPlaneResourceLifecycleOverrideInput = {
  waitingKeepAliveSeconds?: number | null;
  suspendAfterIdleSeconds?: number | null;
  absoluteSessionLifetimeSeconds?: number | null;
  workspaceRetentionDays?: number | null;
  warmPoolMode?: ControlPlaneResourceLifecycleWarmPoolMode | null;
};

export type ControlPlaneResourceLifecycleEffective = {
  waitingKeepAliveSeconds: number;
  suspendAfterIdleSeconds: number;
  absoluteSessionLifetimeSeconds: number | null;
  workspaceRetentionDays: number;
  warmPoolMode: ControlPlaneResourceLifecycleWarmPoolMode;
};

export type ControlPlaneResourceLifecycleConfig = {
  defaults: ControlPlaneResourceLifecycleEffective;
  bounds: ControlPlaneResourceLifecycleBounds;
};

export type ControlPlaneResourceLifecyclePolicy = {
  scope: "tenant" | "project";
  tenantId: string;
  projectId?: string | null;
  overrides: ControlPlaneResourceLifecycleOverrides;
  effective: ControlPlaneResourceLifecycleEffective;
  version: number;
  updatedBy: string | null;
  createdAt: string | null;
  updatedAt: string | null;
};

export type ControlPlaneResourceLifecyclePolicyUpdateInput =
  ControlPlaneResourceLifecycleOverrides & {
    expectedVersion: number;
  };

export type ControlPlaneSchedulingPolicyRule = {
  mode: "any" | "allow";
  values: ReadonlyArray<string>;
};

export type ControlPlaneSchedulingPolicyDocument = {
  denyAll: boolean;
  target: ControlPlaneSchedulingPolicyRule;
  region: ControlPlaneSchedulingPolicyRule;
  cluster: ControlPlaneSchedulingPolicyRule;
  provider: ControlPlaneSchedulingPolicyRule;
  capacityClass: ControlPlaneSchedulingPolicyRule;
};

export type ControlPlaneExecutionSchedulingPolicy = {
  scope: {
    scopeKind: "tenant" | "organization";
    scopeId: string;
    version: number;
    digest: string;
    document: ControlPlaneSchedulingPolicyDocument;
  };
  effective: ControlPlaneSchedulingPolicyDocument;
  parent?: {
    scopeKind: "tenant";
    scopeId: string;
    version: number;
    digest: string;
    document: ControlPlaneSchedulingPolicyDocument;
  };
};

export type ControlPlaneDataResidencyStatement = {
  schemaVersion: "synara-data-residency-statement-v1";
  generatedAt: string;
  tenant: {
    id: string;
    name: string;
    homeRegion: string;
  };
  execution: {
    status: "enforced" | "unrestricted";
    allowedRegions: ReadonlyArray<string>;
    policyVersion: number;
    policyDigest: string;
    enforcement: "candidate-selection-and-commit";
  };
  dataPlanes: {
    metadata: "deployment-annex-required";
    artifacts: "deployment-annex-required";
    kms: "deployment-annex-required";
  };
  limitation: string;
};

/**
 * The server-authoritative data-residency export, including the exact response
 * bytes used for the integrity headers. Consumers that persist or download the
 * statement must use `body` instead of re-serializing `statement`.
 */
export type ControlPlaneDataResidencyStatementDownload = {
  statement: ControlPlaneDataResidencyStatement;
  body: string;
  sha256: string;
  bytes: number;
};

export type ControlPlanePlatformProfile = {
  profile: "personal" | "single-node" | "enterprise";
  metadataStore: "sqlite" | "postgresql";
  artifactStore: "local" | "minio" | "s3";
  queueDriver: "in-process" | "postgres-outbox" | "external";
  controlPlaneReplicas: number;
  highAvailability: boolean;
  leaseEnabled: boolean;
  fencingEnabled: boolean;
  executionTargetKinds: ReadonlyArray<ControlPlaneExecutionTargetKind>;
  artifactPayloadMigration: boolean;
  metadataExportImport: boolean;
  commercializationMode: "internal-self-hosted";
  resourceLifecyclePolicy: ControlPlaneResourceLifecycleConfig;
  internalStatusBoard: {
    configured: boolean;
    url?: string;
  };
};

export function resolveControlPlaneInternalStatusBoardURL(
  profile: Pick<ControlPlanePlatformProfile, "internalStatusBoard"> | null | undefined,
): string | null {
  if (!profile?.internalStatusBoard.configured) return null;
  const candidate = profile.internalStatusBoard.url?.trim();
  if (!candidate?.startsWith("https://")) return null;
  try {
    const parsed = new URL(candidate);
    if (
      parsed.protocol !== "https:" ||
      parsed.host.length === 0 ||
      parsed.username.length > 0 ||
      parsed.password.length > 0 ||
      parsed.search.length > 0 ||
      parsed.hash.length > 0
    ) {
      return null;
    }
    return candidate;
  } catch {
    return null;
  }
}

export type ControlPlaneSessionState = {
  authenticated: true;
  user: {
    userId: string;
    sessionId: string;
    activeTenantId: string | null;
    supportAccessGrantId: string | null;
    audience: "web" | "desktop";
    desktopDeviceId?: string | null;
    email: string;
    displayName: string;
  };
  tenants: ReadonlyArray<ControlPlaneTenantAccess>;
};

export type ControlPlaneOrganization = {
  id: string;
  tenantId: string;
  parentOrganizationId: string | null;
  slug: string;
  name: string;
  kind: "root" | "team" | "department" | "personal";
  status: "active" | "suspended";
  currentUserRole: "owner" | "admin" | "agent_operator" | "member" | "viewer" | null;
  settings: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneTenantMember = {
  tenantId: string;
  userId: string;
  email: string;
  displayName: string;
  role: string;
  status: string;
  joinedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneOutboxMessage = {
  id: string;
  topic: string;
  messageKey: string;
  status: "pending" | "retrying" | "dead-letter" | "published";
  attempts: number;
  availableAt: string;
  createdAt: string;
  claimedAt?: string | null;
  claimExpiresAt?: string | null;
  publishedAt?: string | null;
  deadLetteredAt?: string | null;
  lastError?: string | null;
};

export type ControlPlaneTenantQuota = {
  tenantId: string;
  maxConcurrentExecutions: number | null;
  maxArtifactBytes: number | null;
};

export type ControlPlaneEntitlementValue = {
  kind: "boolean" | "integer" | "string";
  boolean: boolean | null;
  integer: number | null;
  string: string | null;
};

export type ControlPlaneEntitlementSnapshot = {
  tenantId: string;
  profile: {
    code: string;
    displayName: string;
    status: "draft" | "active" | "retired";
    version: number;
    evaluationDays: number;
  };
  profileAssignment: {
    status: "evaluation" | "active" | "suspended" | "disabled";
    version: number;
    evaluationEndsAt: string | null;
    reportingPeriodStart: string;
    reportingPeriodEnd: string;
    assignmentSource: "migration" | "user" | "platform_admin";
  };
  entitlements: Readonly<Record<string, ControlPlaneEntitlementValue>>;
  features: Readonly<Record<string, boolean>>;
};

export type ControlPlaneTenantUsage = {
  tenantId: string;
  entitlementProfileVersion: number;
  periodStart: string;
  periodEnd: string;
  usage: {
    inputTokens: number;
    cachedInputTokens: number;
    outputTokens: number;
    reasoningTokens: number;
    totalTokens: number;
    networkIngressBytes: number;
    networkEgressBytes: number;
    executionSeconds: number;
    providerCostByCurrency: Readonly<Record<string, number>>;
    providerCostReportedCount: number;
    providerCostMissingCount: number;
    platformCharges: ReadonlyArray<{
      kind: string;
      currencyCode: string;
      amountMicros: number;
      source: "estimated" | "actual";
    }>;
    platformCostByCurrency: Readonly<Record<string, number>>;
    knownCostByCurrency: Readonly<Record<string, number>>;
  };
  softQuota: {
    metric: "execution_seconds";
    unit: "seconds";
    limit: number | null;
    observed: number;
    warningPercent: number;
    percentageUsed: number | null;
    state: "unlimited" | "within_limit" | "approaching_limit" | "limit_reached";
    enforcement: "soft";
    hardStop: false;
    recommendedAction: string | null;
  };
  alerts: ReadonlyArray<{
    id: string;
    metric: "execution_seconds";
    thresholdPercent: number;
    severity: "warning" | "limit_reached";
    limit: number;
    observed: number;
    firstObservedAt: string;
    lastObservedAt: string;
  }>;
};

export type ControlPlaneProjectCostAllocation = {
  tenantId: string;
  projectId: string;
  projectName: string;
  organizationId: string;
  costCenterCode: string;
  departmentCode: string;
  version: number;
  updatedBy: string;
  updatedAt: string;
};

export type ControlPlaneInternalCostAllocationRow = ControlPlaneProjectCostAllocation & {
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  networkIngressBytes: number;
  networkEgressBytes: number;
  executionSeconds: number;
  providerCostReportedCount: number;
  providerCostMissingCount: number;
  providerCostByCurrency: Readonly<Record<string, number>>;
  platformCostByCurrency: Readonly<Record<string, number>>;
  knownCostByCurrency: Readonly<Record<string, number>>;
};

export type ControlPlaneInternalCostAllocationReport = {
  tenantId: string;
  entitlementProfileVersion: number;
  periodStart: string;
  periodEnd: string;
  rows: ReadonlyArray<ControlPlaneInternalCostAllocationRow>;
  unallocatedProjectCount: number;
  providerCostByCurrency: Readonly<Record<string, number>>;
  platformCostByCurrency: Readonly<Record<string, number>>;
  knownCostByCurrency: Readonly<Record<string, number>>;
};

export type ControlPlaneSessionUsage = {
  tenantId: string;
  sessionId: string;
  items: ReadonlyArray<{
    executionId: string;
    generation: number;
    turnId: string;
    provider: string;
    model: string | null;
    inputTokens: number;
    cachedInputTokens: number;
    outputTokens: number;
    reasoningTokens: number;
    totalTokens: number;
    networkIngressBytes: number;
    networkEgressBytes: number;
    durationMillis: number;
    providerCostMicros: number;
    providerCostReported: boolean;
    providerCurrency: string;
    platformCharges: ReadonlyArray<{
      kind: string;
      currencyCode: string;
      amountMicros: number;
      source: "estimated" | "actual";
    }>;
    totalCostByCurrency: Readonly<Record<string, number>>;
    costCoverage:
      | "provider-unavailable"
      | "provider-only"
      | "provider-unavailable-with-allocated-platform"
      | "provider-and-allocated-platform";
    final: boolean;
    updatedAt: string;
  }>;
};

export type ControlPlaneTenantSupportPolicy = {
  tenantId: string;
  supportAccessEnabled: boolean;
  version: number;
  reason: string;
  updatedBy: string | null;
  createdAt: string | null;
  updatedAt: string | null;
};

export type ControlPlaneSupportAccessGrant = {
  id: string;
  tenantId: string;
  tenantName: string;
  requesterUserId: string;
  requesterEmail: string;
  requesterDisplayName: string;
  status: "pending" | "active" | "denied" | "revoked" | "expired";
  version: number;
  reason: string;
  requestedDurationSeconds: number;
  requestedAt: string;
  decidedBy: string | null;
  decisionReason: string | null;
  decidedAt: string | null;
  expiresAt: string | null;
  revokedBy: string | null;
  revocationReason: string | null;
  revokedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlanePlatformTenantOverview = {
  operatorRole: "owner" | "admin" | "security_admin";
  generatedAt: string;
  items: ReadonlyArray<{
    id: string;
    slug: string;
    name: string;
    status: "evaluation" | "active" | "suspended" | "closed" | "deleting";
    entitlementProfileCode: string;
    region: string;
    activeMemberCount: number;
    organizationCount: number;
    sessionCount: number;
    executionTargetCount: number;
    workerCount: number;
    offlineWorkerCount: number;
    activeExecutionCount: number;
    queuedExecutionCount: number;
    oldestQueuedAt: string | null;
    failedExecutionCount24h: number;
    artifactCount: number;
    artifactBytes: number;
    pendingArtifactCount: number;
    activeCredentialCount: number;
    unavailableCredentialCount: number;
    activeIdentityConnectionCount: number;
    disabledIdentityConnectionCount: number;
  }>;
};

export type ControlPlaneTenantSupportDiagnostic = {
  schemaVersion: "json-v1";
  generatedAt: string;
  tenant: ControlPlanePlatformTenantOverview["items"][number];
  usage: ControlPlaneTenantUsage;
};

export type ControlPlaneStage6ReleaseApprovalRole =
  | "engineering"
  | "operations"
  | "security"
  | "product"
  | "privacy_legal";

export type ControlPlaneStage6ReleaseState =
  | "draft"
  | "ready_for_review"
  | "approved"
  | "deploying"
  | "observing"
  | "released"
  | "rejected"
  | "rolled_back";

export type ControlPlaneStage6ResidualRisk = {
  id: string;
  summary: string;
  owner: string;
  dueAt: string;
  acceptanceReason: string;
  evidenceReference: string;
};

export type ControlPlaneStage6ReleaseApproval = {
  id: string;
  role: ControlPlaneStage6ReleaseApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  createdAt: string;
};

export type ControlPlaneStage6ReleaseImpactDomain =
  | "code_change"
  | "data_migration"
  | "runtime_isolation"
  | "provider_commercial"
  | "internal_cost"
  | "personal_data"
  | "retention_legal_hold"
  | "data_residency"
  | "regulated_customer"
  | "desktop_distribution"
  | "security_incident";

export type ControlPlaneStage6ReleaseCandidate = {
  id: string;
  candidateId: string;
  sourceCommit: string;
  lockfileSha256: string;
  evidenceBundleSha256: string;
  evidenceBundleSchema: string;
  evidenceBundleAssessment: string;
  evidenceBundleValidatedAt: string;
  evidenceBundleReceiptSizeBytes: number;
  desktopArtifactSetSha256: string;
  evidenceReceiptBound: boolean;
  finalAssetSetSha256: string;
  environmentId: string;
  impactDomains: ReadonlyArray<ControlPlaneStage6ReleaseImpactDomain>;
  providerCommercialAuthorizations: ReadonlyArray<ControlPlaneStage6ReleaseProviderAuthorizationBinding>;
  privacyLegalRequired: boolean;
  requiredApprovalRoles: ReadonlyArray<ControlPlaneStage6ReleaseApprovalRole>;
  state: ControlPlaneStage6ReleaseState;
  version: number;
  createdBy: string;
  decisionSummary: string | null;
  residualRiskDisposition: "none" | "accepted" | null;
  residualRisks: ReadonlyArray<ControlPlaneStage6ResidualRisk>;
  approvals: ReadonlyArray<ControlPlaneStage6ReleaseApproval>;
  finalReview: ControlPlaneStage6ReleaseFinalReview | null;
  approvedAt: string | null;
  releasedAt: string | null;
  rejectedAt: string | null;
  rolledBackAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6ReleaseProviderAuthorizationBinding = {
  authorizationId: string;
  provider: "codex" | "claudeAgent";
  authorizationKey: string;
  authorizationVersion: number;
  currentVersion: number;
  currentState: ControlPlaneProviderCommercialAuthorizationState;
  reviewExpiresAt: string;
  bindingValid: boolean;
  boundAt: string;
};

export type ControlPlaneStage6ReleaseFinalReview = {
  id: string;
  receiptSha256: string;
  schemaVersion: "synara.stage6-final-ga-review-validation.v1";
  assessment: "final-review-consistent-not-ga-authority-verified";
  validatedAt: string;
  receiptSizeBytes: number;
  controlInventorySha256: string;
  controlCount: number;
  finalApprovalCount: number;
  decisionSummary: string;
  residualRiskDisposition: "none" | "accepted";
  residualRisks: ReadonlyArray<ControlPlaneStage6ResidualRisk>;
  allRequiredControlsPassed: boolean;
  allRequiredFinalApprovalsApproved: boolean;
  eligibleForExternalGAAuthorityReview: boolean;
  boundBy: string;
  createdAt: string;
};

export type ControlPlaneStage6ReleaseReadinessGate = {
  id:
    | "candidate_evidence"
    | "provider_commercial_authorizations"
    | "release_approvals"
    | "slo"
    | "recovery"
    | "penetration"
    | "capacity"
    | "incident_exercise"
    | "operations_exercise"
    | "internal_cost"
    | "final_review";
  label: string;
  satisfied: boolean;
  internalAssessment: string;
  externalVerificationRequired: true;
  externalBoundary: string;
};

export type ControlPlaneStage6ReleaseReadiness = {
  candidateRecordId: string;
  candidateId: string;
  candidateState: ControlPlaneStage6ReleaseState;
  internalGatesSatisfied: boolean;
  approvalTransitionEligible: boolean;
  finalReviewGate: ControlPlaneStage6ReleaseReadinessGate;
  releaseTransitionEligible: boolean;
  externalGaStatus: "required_not_verified_by_synara";
  gates: ReadonlyArray<ControlPlaneStage6ReleaseReadinessGate>;
};

export type ControlPlaneCreateStage6ReleaseCandidateInput = {
  candidateId: string;
  sourceCommit: string;
  lockfileSha256: string;
  evidenceBundleSha256: string;
  evidenceBundleReceiptBase64: string;
  finalAssetSetSha256: string;
  environmentId: string;
  impactDomains: ReadonlyArray<ControlPlaneStage6ReleaseImpactDomain>;
  providerCommercialAuthorizationIds?: ReadonlyArray<string>;
};

export type ControlPlaneRecordStage6ReleaseApprovalInput = {
  role: ControlPlaneStage6ReleaseApprovalRole;
  decision: "approved" | "rejected";
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
};

export type ControlPlaneRecordStage6ReleaseFinalReviewInput = {
  receiptSha256: string;
  receiptBase64: string;
};

export type ControlPlaneTransitionStage6ReleaseCandidateInput = {
  expectedVersion: number;
  targetState: Exclude<ControlPlaneStage6ReleaseState, "draft" | "rejected">;
  reason: string;
  decisionSummary?: string;
  residualRiskDisposition?: "none" | "accepted";
  residualRisks?: ReadonlyArray<ControlPlaneStage6ResidualRisk>;
};

export type ControlPlaneStage6IncidentSeverity = "SEV-0" | "SEV-1" | "SEV-2" | "SEV-3";
export type ControlPlaneStage6IncidentState =
  | "investigating"
  | "identified"
  | "monitoring"
  | "resolved"
  | "cancelled";
export type ControlPlaneStage6IncidentComponent =
  | "control-plane-api"
  | "authentication-sso"
  | "execution-scheduling"
  | "worker-runtime"
  | "artifact-service"
  | "web-application";

export type ControlPlaneStage6IncidentOperator = {
  userId: string;
  email: string;
  displayName: string;
};

export type ControlPlaneStage6IncidentInternalUpdate = {
  id: string;
  kind: "initial" | "progress" | "resolved";
  summary: string;
  publishedAt: string;
  evidenceReference: string;
  createdBy: ControlPlaneStage6IncidentOperator;
  createdAt: string;
};

export type ControlPlaneStage6IncidentInternalNotification = {
  id: string;
  kind: "initial" | "progress" | "resolved";
  status: "pending" | "retrying" | "published" | "dead-letter";
  attempts: number;
  availableAt: string;
  publishedAt: string | null;
  deadLetteredAt: string | null;
};

export type ControlPlaneStage6IncidentResolutionApproval = {
  id: string;
  decision: "approved" | "rejected";
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  approver: ControlPlaneStage6IncidentOperator;
  createdAt: string;
};

export type ControlPlaneStage6Incident = {
  id: string;
  incidentKey: string;
  severity: ControlPlaneStage6IncidentSeverity;
  state: ControlPlaneStage6IncidentState;
  title: string;
  internalImpactSummary: string;
  broadInternalImpact: boolean;
  securityPrivacyImpact: boolean;
  internalStatusBoardOrigin: string | null;
  internalStatusBoardIncidentReference: string | null;
  affectedComponents: ReadonlyArray<ControlPlaneStage6IncidentComponent>;
  affectedRegions: ReadonlyArray<string>;
  incidentCommander: ControlPlaneStage6IncidentOperator;
  communicationsLead: ControlPlaneStage6IncidentOperator;
  securityPrivacyLead: ControlPlaneStage6IncidentOperator | null;
  startedAt: string;
  impactConfirmedAt: string;
  cadence: {
    status:
      | "not-broad"
      | "awaiting-status-board"
      | "awaiting-initial"
      | "on-time"
      | "overdue"
      | "resolved";
    firstInternalUpdateTargetAt: string | null;
    nextInternalUpdateTargetAt: string | null;
    firstInternalUpdateWithinTarget: boolean | null;
    overdueSeconds: number;
  };
  internalUpdates: ReadonlyArray<ControlPlaneStage6IncidentInternalUpdate>;
  internalNotifications: ReadonlyArray<ControlPlaneStage6IncidentInternalNotification>;
  resolutionApproval: ControlPlaneStage6IncidentResolutionApproval | null;
  resolutionApprovals: ReadonlyArray<ControlPlaneStage6IncidentResolutionApproval>;
  resolvedAt: string | null;
  cancelledAt: string | null;
  version: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneCreateStage6IncidentInput = {
  incidentKey: string;
  severity: ControlPlaneStage6IncidentSeverity;
  title: string;
  internalImpactSummary: string;
  broadInternalImpact: boolean;
  securityPrivacyImpact: boolean;
  affectedComponents: ReadonlyArray<ControlPlaneStage6IncidentComponent>;
  affectedRegions: ReadonlyArray<string>;
  communicationsLeadUserId: string;
  securityPrivacyLeadUserId?: string;
  startedAt: string;
  impactConfirmedAt: string;
};

export type ControlPlaneAddStage6IncidentInternalUpdateInput = {
  expectedVersion: number;
  kind: "initial" | "progress" | "resolved";
  summary: string;
  publishedAt: string;
  evidenceReference: string;
};

export type ControlPlaneStage6SLOApprovalRole =
  | "engineering"
  | "operations"
  | "security"
  | "product";
export type ControlPlaneStage6SLOObjective = {
  key: "availability" | "apiLatency" | "executionStartDelay" | "eventDelay";
  targetRatio: number;
  goodRatio: number;
  sampleCount: number;
  errorBudgetRemainingRatio: number;
  policyState:
    | "normal-delivery"
    | "risk-note-required"
    | "risky-rollout-paused"
    | "reliability-freeze";
  assessable: boolean;
  objectiveMet: boolean;
};
export type ControlPlaneStage6SLOApproval = {
  id: string;
  role: ControlPlaneStage6SLOApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6SLOWindow = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  windowId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  publicOrigin: string;
  windowStartedAt: string;
  windowCompletedAt: string;
  validatedAt: string;
  queryRevision: string;
  allObjectivesAssessable: boolean;
  allObjectivesMet: boolean;
  eligibleForHumanGateReview: boolean;
  worstBudgetRemainingRatio: number;
  budgetPolicyState: ControlPlaneStage6SLOObjective["policyState"];
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  objectives: ReadonlyArray<ControlPlaneStage6SLOObjective>;
  approvals: ReadonlyArray<ControlPlaneStage6SLOApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6RecoveryApprovalRole =
  | "database"
  | "kms"
  | "operations"
  | "security"
  | "storage";
export type ControlPlaneStage6RecoveryComponent = {
  key: "kms" | "object-storage" | "postgresql" | "queue";
  profile: string;
  sourceRegion: string;
  restoreRegion: string;
  measuredRpoSeconds: number;
  rpoObjectiveSeconds: number;
  measuredRtoSeconds: number;
  rtoObjectiveSeconds: number;
  rpoWithinObjective: boolean;
  rtoWithinObjective: boolean;
  restoreServedCanary: boolean;
};
export type ControlPlaneStage6RecoveryApproval = {
  id: string;
  role: ControlPlaneStage6RecoveryApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6RecoveryDrill = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  drillId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  candidateBindingSha256: string;
  recoverySubjectSha256: string;
  startedAt: string;
  completedAt: string;
  validatedAt: string;
  measurementsWithinObjectives: boolean;
  allRestoreCanariesPassed: boolean;
  allSourceApprovalsApproved: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  externalAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  components: ReadonlyArray<ControlPlaneStage6RecoveryComponent>;
  approvals: ReadonlyArray<ControlPlaneStage6RecoveryApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6PenetrationApprovalRole = "engineering" | "product" | "security";
export type ControlPlaneStage6PenetrationAsset = {
  assetType: "control-plane-api" | "provider-host" | "web" | "worker-runtime";
  artifactSha256: string;
  tested: boolean;
};
export type ControlPlaneStage6PenetrationApproval = {
  id: string;
  role: ControlPlaneStage6PenetrationApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6PenetrationEngagement = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  engagementId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  deploymentProfile: string;
  startedAt: string;
  completedAt: string;
  reportIssuedAt: string;
  validatedAt: string;
  thirdPartyIndependenceDeclared: boolean;
  stage5DependencySatisfied: boolean;
  assetCoverageComplete: boolean;
  scopeCoverageComplete: boolean;
  methodologyCoverageComplete: boolean;
  noUnacceptedHighOrCriticalFindings: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  externalAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  assets: ReadonlyArray<ControlPlaneStage6PenetrationAsset>;
  approvals: ReadonlyArray<ControlPlaneStage6PenetrationApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6CapacityApprovalRole = "engineering" | "operations";
export type ControlPlaneStage6CapacityPhase = {
  name: "steady-peak" | "burst" | "tenant-hotspot" | "rolling-disruption" | "cooldown";
  startedAt: string;
  completedAt: string;
  durationSeconds: number;
  loadMultiplier: number;
};
export type ControlPlaneStage6CapacityApproval = {
  id: string;
  role: ControlPlaneStage6CapacityApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6CapacityRun = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  runId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  startedAt: string;
  completedAt: string;
  validatedAt: string;
  durationSeconds: number;
  minimumDurationSeconds: number;
  sampleIntervalSeconds: number;
  externalProbeRegions: number;
  externalProbeCoverageRatio: number;
  forecastHeadroomCovered: boolean;
  phaseCoverageComplete: boolean;
  exerciseCoverageComplete: boolean;
  measurementsWithinObjectives: boolean;
  releaseEligibleEnvironment: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  externalAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  phases: ReadonlyArray<ControlPlaneStage6CapacityPhase>;
  approvals: ReadonlyArray<ControlPlaneStage6CapacityApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6IncidentExerciseApprovalRole = "operations" | "communications";
export type ControlPlaneStage6IncidentExerciseApproval = {
  id: string;
  role: ControlPlaneStage6IncidentExerciseApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6IncidentExercise = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  exerciseId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  exerciseMode: "live-internal-communication";
  severity: "SEV-0" | "SEV-1" | "SEV-2";
  serviceOrigin: string;
  internalStatusBoardOrigin: string;
  startedAt: string;
  completedAt: string;
  validatedAt: string;
  independentInternalStatusBoardDeclared: boolean;
  roleSeparationComplete: boolean;
  pagingExerciseComplete: boolean;
  internalStatusBoardComponentsComplete: boolean;
  internalTimelineWithinTargets: boolean;
  employeeNotificationDeliveryComplete: boolean;
  recoveryVerificationComplete: boolean;
  reviewComplete: boolean;
  releaseEligibleEnvironment: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  deploymentAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  approvals: ReadonlyArray<ControlPlaneStage6IncidentExerciseApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6OperationsExerciseApprovalRole = "operations" | "security";
export type ControlPlaneStage6OperationsExerciseApproval = {
  id: string;
  role: ControlPlaneStage6OperationsExerciseApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6OperationsExercise = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  matrixSha256: string;
  webOrigin: string;
  adminOrigin: string;
  startedAt: string;
  completedAt: string;
  validatedAt: string;
  accountCount: number;
  operationCount: number;
  matrixProfile: "internal-self-hosted-v3" | "legacy-commercial-v2";
  evidenceFileCount: number;
  allOperationsPassed: boolean;
  allNegativeAuthorizationsDenied: boolean;
  noDeveloperFallbacks: boolean;
  productionAuthenticationDeclared: boolean;
  supportLifecycleComplete: boolean;
  receiptApprovalsComplete: boolean;
  releaseEligibleEnvironment: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  externalAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  approvals: ReadonlyArray<ControlPlaneStage6OperationsExerciseApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6InternalCostApprovalRole = "operations" | "owner";
export type ControlPlaneStage6InternalCostApproval = {
  id: string;
  role: ControlPlaneStage6InternalCostApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  approverEmail: string;
  approverName: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};
export type ControlPlaneStage6InternalCostReview = {
  id: string;
  candidateRecordId: string;
  candidateId: string;
  receiptSha256: string;
  receiptSizeBytes: number;
  releaseCommit: string;
  environmentClass: "production" | "production-like";
  environmentId: string;
  manifestSha256: string;
  controlPlaneOrigin: string;
  migrationName: string;
  migrationSha256: string;
  periodStart: string;
  periodEnd: string;
  validatedAt: string;
  executionCount: number;
  inputTokens: number;
  outputTokens: number;
  cachedInputTokens: number;
  cacheCreationInputTokens: number;
  providerCostReportedExecutionCount: number;
  providerCostUnavailableExecutionCount: number;
  actualPlatformAllocationCount: number;
  estimatedPlatformAllocationCount: number;
  providerCostByCurrency: Readonly<Record<string, number>>;
  platformCostByCurrency: Readonly<Record<string, number>>;
  knownCostByCurrency: Readonly<Record<string, number>>;
  evidenceFileCount: number;
  tokenTotalsReconciled: boolean;
  providerCoverageComplete: boolean;
  actualOverridesEstimate: boolean;
  currencySafeAggregation: boolean;
  tenantIsolationValidated: boolean;
  noPaymentDataPresent: boolean;
  eligibleForHumanGateReview: boolean;
  cryptographicSignaturesVerified: boolean;
  externalSourceAndAuthorityVerificationRequired: boolean;
  state: "recorded" | "approved" | "rejected";
  version: number;
  createdBy: string;
  approvals: ReadonlyArray<ControlPlaneStage6InternalCostApproval>;
  approvedAt: string | null;
  rejectedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneStage6ComplianceFramework = "soc2_type2" | "iso27001";
export type ControlPlaneStage6ComplianceState = "draft" | "ready_for_review" | "record_complete";
export type ControlPlaneStage6ComplianceFamily =
  | "logical_access"
  | "change_release"
  | "operations"
  | "data_governance"
  | "resilience"
  | "vendor_provider"
  | "security_testing";
export type ControlPlaneStage6ComplianceDecisionRole =
  | "security"
  | "operations"
  | "legal_privacy"
  | "executive";

export type ControlPlaneStage6ComplianceEvidenceReview = {
  id: string;
  decision: "accepted" | "rejected";
  reviewRole: "security" | "operations" | "legal_privacy" | "auditor";
  reviewerUserId: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  createdAt: string;
};

export type ControlPlaneStage6ComplianceEvidence = {
  id: string;
  evidenceId: string;
  evidenceType:
    | "release_manifest"
    | "access_review"
    | "release"
    | "incident"
    | "recovery"
    | "vendor_review"
    | "security_test"
    | "data_governance"
    | "other";
  periodStart: string;
  periodEnd: string;
  sourceReference: string;
  sha256: string;
  mediaType: string;
  classification: "internal" | "confidential" | "restricted";
  collectedAt: string;
  retentionUntil: string;
  submittedBy: string;
  review: ControlPlaneStage6ComplianceEvidenceReview | null;
  createdAt: string;
};

export type ControlPlaneStage6ComplianceControl = {
  id: string;
  controlId: string;
  family: ControlPlaneStage6ComplianceFamily;
  title: string;
  description: string;
  ownerUserId: string;
  cadence:
    | "continuous"
    | "daily"
    | "monthly"
    | "quarterly"
    | "annual"
    | "per_release"
    | "per_incident";
  evidenceRequirement: string;
  createdBy: string;
  evidence: ReadonlyArray<ControlPlaneStage6ComplianceEvidence>;
  createdAt: string;
};

export type ControlPlaneStage6ComplianceDecision = {
  id: string;
  decisionRole: ControlPlaneStage6ComplianceDecisionRole;
  decision: "approved" | "rejected";
  deciderUserId: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  supersededAt: string | null;
  supersededReason: string | null;
  createdAt: string;
};

export type ControlPlaneStage6ComplianceProgram = {
  id: string;
  programKey: string;
  framework: ControlPlaneStage6ComplianceFramework;
  scopeVersion: string;
  scopeSummary: string;
  executiveSponsorUserId: string;
  auditorOrganization: string;
  auditorEngagementReference: string;
  observationStart: string;
  observationEnd: string;
  evidenceRepositoryReference: string;
  evidenceAccessPolicyReference: string;
  evidenceRetentionDays: number;
  vendorRegisterReference: string;
  riskRegisterReference: string;
  state: ControlPlaneStage6ComplianceState;
  version: number;
  createdBy: string;
  recordCompletedAt: string | null;
  controls: ReadonlyArray<ControlPlaneStage6ComplianceControl>;
  decisions: ReadonlyArray<ControlPlaneStage6ComplianceDecision>;
  readiness: {
    assessment: "record-incomplete-not-audit-active" | "record-complete-not-audit-active";
    missingControlFamilies: ReadonlyArray<ControlPlaneStage6ComplianceFamily>;
    missingDecisionRoles: ReadonlyArray<ControlPlaneStage6ComplianceDecisionRole>;
    hasAcceptedReleaseManifest: boolean;
    eligibleForRecordCompleteReview: boolean;
  };
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneCreateStage6ComplianceProgramInput = {
  programKey: string;
  framework: ControlPlaneStage6ComplianceFramework;
  scopeVersion: string;
  scopeSummary: string;
  executiveSponsorUserId: string;
  auditorOrganization: string;
  auditorEngagementReference: string;
  observationStart: string;
  observationEnd: string;
  evidenceRepositoryReference: string;
  evidenceAccessPolicyReference: string;
  evidenceRetentionDays: number;
  vendorRegisterReference: string;
  riskRegisterReference: string;
};

export type ControlPlaneCreateStage6ComplianceControlInput = {
  controlId: string;
  family: ControlPlaneStage6ComplianceFamily;
  title: string;
  description: string;
  ownerUserId: string;
  cadence: ControlPlaneStage6ComplianceControl["cadence"];
  evidenceRequirement: string;
};

export type ControlPlaneSubmitStage6ComplianceEvidenceInput = {
  controlRecordId: string;
  evidenceId: string;
  evidenceType: ControlPlaneStage6ComplianceEvidence["evidenceType"];
  periodStart: string;
  periodEnd: string;
  sourceReference: string;
  sha256: string;
  mediaType: string;
  classification: ControlPlaneStage6ComplianceEvidence["classification"];
  collectedAt: string;
  retentionUntil: string;
};

export type ControlPlaneReviewStage6ComplianceEvidenceInput = {
  decision: "accepted" | "rejected";
  reviewRole: ControlPlaneStage6ComplianceEvidenceReview["reviewRole"];
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
};

export type ControlPlaneRecordStage6ComplianceDecisionInput = {
  decisionRole: ControlPlaneStage6ComplianceDecisionRole;
  decision: "approved" | "rejected";
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
};

export type ControlPlaneTransitionStage6ComplianceProgramInput = {
  expectedVersion: number;
  targetState: Exclude<ControlPlaneStage6ComplianceState, "draft">;
  reason: string;
};

export type ControlPlaneProviderCommercialAuthorizationState =
  | "draft"
  | "ready_for_review"
  | "active"
  | "rejected"
  | "revoked";
export type ControlPlaneProviderCommercialApprovalRole =
  | "legal"
  | "privacy"
  | "security"
  | "product";

export type ControlPlaneProviderCommercialApproval = {
  id: string;
  role: ControlPlaneProviderCommercialApprovalRole;
  decision: "approved" | "rejected";
  approverUserId: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  createdAt: string;
};

export type ControlPlaneProviderCommercialAuthorization = {
  id: string;
  authorizationKey: string;
  provider: "codex" | "claudeAgent";
  providerProduct: string;
  accountType: string;
  contractingEntity: string;
  credentialMode: "customer_byok" | "platform_managed";
  allowedCredentialScopes: ReadonlyArray<"user" | "organization" | "tenant" | "platform">;
  allowedRegions: ReadonlyArray<string>;
  dataUsePolicy: "no_training" | "tenant_explicit_opt_in";
  retentionPolicy: string;
  termsEffectiveAt: string;
  termsReference: string;
  termsSha256: string | null;
  agreementReference: string;
  agreementSha256: string | null;
  dpaReference: string;
  dpaSha256: string | null;
  prohibitedUseSummary: string;
  terminationRunbookReference: string;
  terminationRunbookSha256: string | null;
  reviewExpiresAt: string;
  state: ControlPlaneProviderCommercialAuthorizationState;
  version: number;
  createdBy: string;
  approvals: ReadonlyArray<ControlPlaneProviderCommercialApproval>;
  activatedAt: string | null;
  rejectedAt: string | null;
  revokedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneCreateProviderCommercialAuthorizationInput = {
  authorizationKey: string;
  provider: "codex" | "claudeAgent";
  providerProduct: string;
  accountType: string;
  contractingEntity: string;
  credentialMode: "customer_byok" | "platform_managed";
  allowedCredentialScopes: ReadonlyArray<"user" | "organization" | "tenant" | "platform">;
  allowedRegions: ReadonlyArray<string>;
  dataUsePolicy: "no_training" | "tenant_explicit_opt_in";
  retentionPolicy: string;
  termsEffectiveAt: string;
  termsReference: string;
  termsSha256: string;
  agreementReference: string;
  agreementSha256: string;
  dpaReference: string;
  dpaSha256: string;
  prohibitedUseSummary: string;
  terminationRunbookReference: string;
  terminationRunbookSha256: string;
  reviewExpiresAt: string;
};

export type ControlPlaneRecordProviderCommercialApprovalInput = {
  role: ControlPlaneProviderCommercialApprovalRole;
  decision: "approved" | "rejected";
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
};

export type ControlPlaneTransitionProviderCommercialAuthorizationInput = {
  expectedVersion: number;
  targetState: Exclude<ControlPlaneProviderCommercialAuthorizationState, "draft" | "rejected">;
  reason: string;
};

export type ControlPlaneGovernanceAuthorityKey =
  | "release.engineering"
  | "release.operations"
  | "release.security"
  | "release.product"
  | "release.privacy_legal"
  | "compliance.security"
  | "compliance.operations"
  | "compliance.legal_privacy"
  | "compliance.executive"
  | "compliance.evidence.security"
  | "compliance.evidence.operations"
  | "compliance.evidence.legal_privacy"
  | "compliance.evidence.auditor"
  | "provider_commercial.legal"
  | "provider_commercial.privacy"
  | "provider_commercial.security"
  | "provider_commercial.product"
  | "recovery.database"
  | "recovery.kms"
  | "recovery.operations"
  | "recovery.security"
  | "recovery.storage"
  | "penetration.engineering"
  | "penetration.product"
  | "penetration.security"
  | "capacity.engineering"
  | "capacity.operations"
  | "incident_exercise.operations"
  | "incident_exercise.communications"
  | "operations_exercise.operations"
  | "operations_exercise.security"
  | "internal_cost.operations"
  | "internal_cost.owner";

export type ControlPlaneGovernanceAuthorityGrant = {
  id: string;
  userId: string;
  userEmail: string;
  userDisplayName: string;
  authorityKey: ControlPlaneGovernanceAuthorityKey;
  status: "active" | "revoked";
  version: number;
  expiresAt: string;
  grantedBy: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string | null;
  revokedAt: string | null;
  revokedBy: string | null;
  revocationReason: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneGovernanceAuthorityOperator = {
  userId: string;
  email: string;
  displayName: string;
  role: "owner" | "admin" | "security_admin";
};

export type ControlPlaneCreateGovernanceAuthorityInput = {
  userId: string;
  authorityKey: ControlPlaneGovernanceAuthorityKey;
  expiresAt: string;
  reason: string;
  evidenceReference: string;
  evidenceSha256: string;
};

export type ControlPlaneDesktopSubject = {
  userId: string;
  email: string;
  displayName: string;
  role: string;
  membershipStatus: string;
  selfEnrollable: boolean;
};

export type ControlPlaneDesktopEnrollment = {
  id: string;
  status: "pending" | "redeemed" | "expired" | "revoked";
  version: number;
  mode: "connect_existing" | "provisioned_then_connect";
  authority: "self";
  controlPlaneOrigin: string;
  issuedByUserId: string;
  issuerEmail: string;
  subjectUserId: string;
  subjectEmail: string;
  subjectDisplayName: string;
  tenantId: string;
  organizationId: string | null;
  reason: string;
  expiresAt: string;
  openedAt: string | null;
  redeemedAt: string | null;
  redeemedDeviceId: string | null;
  revokedAt: string | null;
  terminalReason: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneDesktopDevice = {
  id: string;
  userId: string;
  userEmail: string;
  userDisplayName: string;
  defaultTenantId: string;
  defaultOrganizationId: string | null;
  platform: "darwin" | "win32" | "linux";
  appVersion: string;
  deviceLabel: string;
  status: "active" | "revoked";
  version: number;
  lastSeenAt: string;
  revokedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlanePlatformDesktopAccess = {
  controlPlaneOrigin: string;
  enrollmentTtlSeconds: number;
  subjects: ReadonlyArray<ControlPlaneDesktopSubject>;
  enrollments: ReadonlyArray<ControlPlaneDesktopEnrollment>;
  devices: ReadonlyArray<ControlPlaneDesktopDevice>;
};

export type ControlPlaneIssueDesktopEnrollmentInput = {
  subjectUserId: string;
  organizationId?: string;
  mode: "connect_existing" | "provisioned_then_connect";
  reason: string;
};

export type ControlPlaneIssuedDesktopEnrollment = {
  enrollment: ControlPlaneDesktopEnrollment;
  handle: string;
};

export type ControlPlaneRetentionPolicy = {
  tenantId: string;
  sessionArchiveAfterDays: number | null;
  artifactDeleteAfterDays: number | null;
  updatedBy: string;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneLegalHold = {
  id: string;
  tenantId: string;
  scopeType: "tenant" | "user" | "organization" | "project" | "session";
  scopeId: string;
  name: string;
  matterReference: string;
  reason: string;
  status: "active" | "released";
  version: number;
  createdBy: string;
  createdAt: string;
  releasedBy: string | null;
  releaseReason: string | null;
  releasedAt: string | null;
  updatedAt: string;
};

export type ControlPlanePrivacyRequest = {
  id: string;
  tenantId: string;
  subjectUserId: string;
  requestType: "access_export" | "erasure";
  status:
    | "requested"
    | "verified"
    | "approved"
    | "processing"
    | "completed"
    | "denied"
    | "cancelled"
    | "failed";
  version: number;
  requestedBy: string;
  intakeReason: string;
  dueAt: string;
  lastTransitionBy: string;
  lastTransitionReason: string;
  completedAt: string | null;
  resultSummary: Readonly<Record<string, unknown>>;
  resultDigestSha256: string | null;
  createdAt: string;
  updatedAt: string;
  events?: ReadonlyArray<{
    id: string;
    version: number;
    fromStatus: string | null;
    toStatus: string;
    actorUserId: string;
    reason: string;
    metadata: Readonly<Record<string, unknown>>;
    occurredAt: string;
  }>;
};

export type ControlPlanePrivacyExportResult = {
  request: ControlPlanePrivacyRequest;
  bundle: Readonly<Record<string, unknown>>;
};

export type ControlPlaneTenantDataExportResult = {
  receipt: {
    id: string;
    tenantId: string;
    requestedBy: string;
    schemaVersion: string;
    digestSha256: string;
    byteCount: number;
    rowCounts: Readonly<Record<string, unknown>>;
    createdAt: string;
  };
  bundle: Readonly<Record<string, unknown>>;
};

export type ControlPlaneIdentityConnection = {
  id: string;
  tenantId: string;
  kind: "oidc" | "saml";
  name: string;
  status: "active" | "disabled";
  issuer: string;
  clientId: string | null;
  configuration: {
    scopes?: ReadonlyArray<string>;
    allowedDomains?: ReadonlyArray<string>;
    groupsClaim?: string;
    defaultTenantRole?: string;
    metadataUrl?: string;
    entityId?: string;
    emailAttribute?: string;
    displayNameAttribute?: string;
    groupsAttribute?: string;
  };
  createdAt: string;
  updatedAt: string;
};

export type ControlPlanePublicIdentityConnection = Pick<
  ControlPlaneIdentityConnection,
  "id" | "tenantId" | "kind" | "name"
>;

export type ControlPlaneIdentityGroupMapping = {
  id: string;
  externalGroup: string;
  tenantRole: string | null;
  organizationId: string | null;
  organizationRole: string | null;
};

export type ControlPlaneIdentityDomain = {
  id: string;
  tenantId: string;
  domain: string;
  status: "pending" | "verified" | "revoked";
  verificationRecordName: string;
  verificationExpiresAt: string;
  verifiedAt: string | null;
  revokedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneIdentityDomainChallenge = ControlPlaneIdentityDomain & {
  verificationRecordValue: string;
};

export type ControlPlaneTenantIdentityPolicy = {
  tenantId: string;
  ssoEnforcement: "optional" | "required";
  version: number;
  recoveryUserId: string | null;
  enforcementSetAt: string | null;
  updatedAt: string | null;
};

export type ControlPlaneServiceAccount = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  name: string;
  description: string;
  status: "active" | "revoked";
  role:
    | "owner"
    | "admin"
    | "security_admin"
    | "cost_admin"
    | "auditor"
    | "agent_operator"
    | "member"
    | "viewer";
  scopes: ReadonlyArray<string>;
  rateLimitPerMinute: number;
  lastUsedAt: string | null;
  createdAt: string;
  updatedAt: string;
  revokedAt: string | null;
};

export type ControlPlaneIssuedServiceAccount = {
  account: ControlPlaneServiceAccount;
  token: string;
};

export type ControlPlaneServiceAccountUsageSummary = {
  routePattern: string;
  admittedCount: number;
  rateLimitedCount: number;
  requestCount: number;
  successCount: number;
  clientErrorCount: number;
  serverErrorCount: number;
  totalDurationMs: number;
};

export type ControlPlaneServiceAccountUsageReport = {
  tenantId: string;
  serviceAccountId: string;
  organizationId: string | null;
  from: string;
  to: string;
  items: ReadonlyArray<ControlPlaneServiceAccountUsageSummary>;
};

export type ControlPlaneDeveloperWebhookEventType =
  | "turn.completed"
  | "request.opened"
  | "approval.requested"
  | "execution.completed"
  | "execution.failed"
  | "execution.cancelled"
  | "execution.interrupted"
  | "execution.suspended";

export type ControlPlaneDeveloperWebhook = {
  id: string;
  tenantId: string;
  name: string;
  url: string;
  status: "active" | "disabled" | "revoked";
  eventTypes: ReadonlyArray<ControlPlaneDeveloperWebhookEventType>;
  secretVersion: number;
  lastDeliveredAt: string | null;
  lastFailedAt: string | null;
  lastFailureSummary: string | null;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  revokedAt: string | null;
};

export type ControlPlaneIssuedDeveloperWebhook = {
  endpoint: ControlPlaneDeveloperWebhook;
  secret: string;
};

export type ControlPlaneDeveloperWebhookDelivery = {
  id: string;
  endpointId: string;
  sessionEventId: string;
  eventType: ControlPlaneDeveloperWebhookEventType;
  sessionId: string;
  executionId: string | null;
  sequence: number;
  status: "pending" | "retrying" | "published" | "dead-letter";
  attempts: number;
  availableAt: string;
  createdAt: string;
  publishedAt: string | null;
  deadLetteredAt: string | null;
  lastError: string | null;
};

export type ControlPlaneCredentialPurpose = "provider" | "git" | "registry" | "package";
export type ControlPlaneCredentialScope = "user" | "organization" | "tenant" | "platform";

export type ControlPlaneCredential = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  scope: ControlPlaneCredentialScope;
  scopeUserId: string | null;
  selectorOrganizationId: string | null;
  selectorModel: string | null;
  autoSelectEnabled: boolean;
  name: string;
  purpose: ControlPlaneCredentialPurpose;
  provider: string;
  credentialType: string;
  kmsProvider: string;
  kmsKeyId: string;
  version: number;
  createdBy: string;
  updatedBy: string;
  createdAt: string;
  updatedAt: string;
  expiresAt: string | null;
  revokedAt: string | null;
};

export type ControlPlaneProviderCredentialScopePolicy = {
  tenantId: string;
  platformCredentialsEnabled: boolean;
  platformCredentialAutoSelect: boolean;
  updatedBy: string | null;
  createdAt: string | null;
  updatedAt: string | null;
};

export type ControlPlaneCredentialBindingKind =
  | "git_fetch"
  | "git_push"
  | "registry_pull"
  | "registry_push"
  | "package_read"
  | "package_publish"
  | "worker_image_pull";

export type ControlPlaneCredentialBinding = {
  id: string;
  tenantId: string;
  organizationId: string | null;
  projectId: string | null;
  executionTargetId: string | null;
  credentialId: string;
  bindingKind: ControlPlaneCredentialBindingKind;
  selector: string;
  createdBy: string;
  createdAt: string;
  disabledAt: string | null;
  disabledBy: string | null;
};

export type ControlPlaneAuditLogEntry = {
  eventId: string;
  tenantId: string;
  actorType: "user" | "service_account" | "worker" | "system";
  actorId: string | null;
  action: string;
  resourceType: string;
  resourceId: string | null;
  organizationId: string | null;
  requestId: string;
  metadata: Record<string, unknown>;
  occurredAt: string;
};

export type ControlPlaneAuditLogFilters = {
  action?: string;
  actorType?: ControlPlaneAuditLogEntry["actorType"] | "";
  resourceType?: string;
  organizationId?: string;
  occurredAfter?: string;
  occurredBefore?: string;
};

export type ControlPlaneAuditLogPage = {
  items: ReadonlyArray<ControlPlaneAuditLogEntry>;
  nextCursor: string | null;
};

export type ControlPlaneProject = {
  id: string;
  tenantId: string;
  organizationId: string;
  name: string;
  repositoryUrl: string | null;
  defaultBranch: string;
  gitCredentialId: string | null;
  visibility: "private" | "organization" | "tenant";
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneAgentSession = {
  id: string;
  tenantId: string;
  organizationId: string;
  projectId: string;
  createdBy: string;
  title: string;
  status: "active" | "suspended" | "archived";
  visibility: "private" | "project" | "organization";
  provider: ProviderKind;
  model: string | null;
  providerCredentialId: string | null;
  executionTargetId: string;
  requestedExecutionTargetId?: string;
  executionTargetGroupId?: string;
  routingPolicyVersion?: number;
  preferredExecutionRegion?: string;
  lastEventSequence: number;
  resourceState:
    | "idle"
    | "provisioning"
    | "active"
    | "waiting"
    | "checkpointing"
    | "suspended"
    | "restoring"
    | "terminating";
  meaningfulActivityAt: string;
  resourceIdleSince: string | null;
  absoluteExpiresAt: string | null;
  resourceLifecyclePolicy: ControlPlaneResourceLifecycleEffective;
  createdAt: string;
  updatedAt: string;
  archivedAt: string | null;
};

export type ControlPlaneExecutionTargetKind = "local" | "ssh" | "docker" | "kubernetes";

export type ControlPlaneIsolationProfile =
  | "single-tenant-trusted-v1"
  | "kubernetes-restricted-v1"
  | "gvisor-sandboxed-v1"
  | "microvm-isolated-v1";

export type ControlPlaneRuntimeIsolationPolicy = {
  mode: "explicit" | "auto" | "legacy-native";
  requestedRuntime: "auto" | "runc" | "gvisor" | "firecracker";
  preferred: ReadonlyArray<"runc" | "gvisor" | "firecracker">;
  minimumProfile: ControlPlaneIsolationProfile;
  fallbackPolicy: "fail-closed" | "allow-lower";
  runtimeClassName?: string;
  gvisorCompatibleProviders: ReadonlyArray<ProviderHostProviderKind>;
};

export type ControlPlaneRuntimeIsolationPolicyInput =
  | {
      mode: "explicit";
      runtime: "runc" | "gvisor" | "firecracker";
      minimumProfile: ControlPlaneIsolationProfile;
      fallbackPolicy: "fail-closed" | "allow-lower";
      runtimeClassName?: string;
      gvisorCompatibleProviders?: ReadonlyArray<ProviderHostProviderKind>;
    }
  | {
      mode: "auto";
      preferred: ReadonlyArray<"runc" | "gvisor" | "firecracker">;
      minimumProfile: ControlPlaneIsolationProfile;
      fallbackPolicy: "fail-closed" | "allow-lower";
      runtimeClassName?: string;
      gvisorCompatibleProviders?: ReadonlyArray<ProviderHostProviderKind>;
    };

export type ControlPlaneRuntimeIsolationStatus = {
  state: "available" | "degraded" | "unattested" | "stale";
  detectedRuntimes: ReadonlyArray<"runc" | "gvisor" | "firecracker">;
  detectedProfiles: ReadonlyArray<ControlPlaneIsolationProfile>;
  reasonCode: string | null;
  observedAt: string;
  expiresAt: string;
  runningGeneration?: {
    generation: number;
    effectiveRuntime: "runc" | "gvisor" | "firecracker";
    effectiveProfile: ControlPlaneIsolationProfile;
    decision: "selected" | "fallback";
    policySource: "legacy-native" | "target-explicit" | "target-auto" | "lane-policy";
  };
};

export type ControlPlaneTenantUsageDownload = {
  usage: ControlPlaneTenantUsage;
  body: string;
  sha256: string;
  bytes: number;
};

export type ControlPlaneTenantSupportDiagnosticDownload = {
  diagnostic: ControlPlaneTenantSupportDiagnostic;
  body: string;
  sha256: string;
  bytes: number;
};

export type ControlPlaneExecutionTarget = {
  id: string;
  tenantId: string | null;
  organizationId: string | null;
  kind: ControlPlaneExecutionTargetKind;
  name: string;
  status: "active" | "disabled" | "offline";
  capabilities: Record<string, unknown>;
  isolationProfile: ControlPlaneIsolationProfile;
  platformSharedEligible: boolean;
  productBoundary: "single-tenant-trusted" | "multi-tenant-restricted";
  runtimeIsolationPolicy?: ControlPlaneRuntimeIsolationPolicy;
  runtimeIsolationStatus?: ControlPlaneRuntimeIsolationStatus;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneWorkerPoolMode = "resident" | "per-execution" | "warm";
export type ControlPlaneWorkerPoolStatus = "active" | "draining" | "disabled";
export type ControlPlaneWorkerPoolCapacityClass = "standard" | "interactive";
export type ControlPlaneWorkerPoolTenantIsolation = "pinned" | "shared";

export type ControlPlaneWorkerPool = {
  id: string;
  tenantId: string | null;
  executionTargetId: string;
  name: string;
  mode: ControlPlaneWorkerPoolMode;
  capacityClass: ControlPlaneWorkerPoolCapacityClass;
  tenantIsolation: ControlPlaneWorkerPoolTenantIsolation;
  clusterId: string;
  region: string;
  namespace: string;
  desiredIdleUnits: number;
  minIdleUnits: number;
  maxActiveUnits: number;
  schedulingTemplate: Record<string, unknown>;
  status: ControlPlaneWorkerPoolStatus;
  version: number;
  createdAt: string;
  updatedAt: string;
};

export type ControlPlaneExecutionPlacementPolicy = {
  tenantId: string | null;
  executionTargetId: string;
  version: number;
  defaultPoolId: string;
  balancedPoolId: string | null;
  lowLatencyPoolId: string | null;
  updatedBy: string | null;
  updatedAt: string;
};

export type ControlPlaneExecutionPlacementState = {
  pools: ReadonlyArray<ControlPlaneWorkerPool>;
  policy: ControlPlaneExecutionPlacementPolicy;
};

export type ControlPlaneWorkerPoolInput = {
  name: string;
  mode: ControlPlaneWorkerPoolMode;
  capacityClass: ControlPlaneWorkerPoolCapacityClass;
  tenantIsolation: ControlPlaneWorkerPoolTenantIsolation;
  clusterId: string;
  region: string;
  namespace: string;
  desiredIdleUnits: number;
  minIdleUnits: number;
  maxActiveUnits: number;
  schedulingTemplate: Record<string, unknown>;
  status: ControlPlaneWorkerPoolStatus;
};

export type ControlPlaneWorker = {
  id: string;
  incarnation: number;
  instanceUid: string;
  executionTargetId: string;
  targetKind: ControlPlaneExecutionTargetKind | string;
  workerMode: "execution-pinned" | "warm-pool" | "general-pool";
  tenantBindingId?: string | null;
  clusterId: string;
  namespace: string;
  podName: string;
  version: string;
  protocolVersion: number;
  currentManifestId?: string | null;
  compatibilityStatus: string;
  compatibilityReason?: string | null;
  compatibilityCheckedAt?: string | null;
  workerReleaseRevisionId?: string | null;
  workerReleaseChannel?: string | null;
  workerReleaseStatus: string;
  workerReleaseReason?: string | null;
  workerReleaseCheckedAt?: string | null;
  leaseSupported: boolean;
  fencingSupported: boolean;
  status: string;
  administrativeStatus: string;
  registeredAt: string;
  lastHeartbeatAt: string;
  drainingAt?: string | null;
  reconciliationDrainIncarnation?: number | null;
  reconciliationDrainInstanceUid?: string | null;
  reconciliationDrainRequestedAt?: string | null;
  reconciliationDrainReason?: string | null;
  terminatedAt?: string | null;
  revokedAt?: string | null;
  revokedBy?: string | null;
  revocationReason?: string | null;
};

export type ControlPlaneWorkerRevocationResult = {
  worker: ControlPlaneWorker;
  releasedExecutionLeases: number;
  recoveringExecutions: number;
  outcomeUnknownExecutions: number;
  checkpointUnconfirmedExecutions: number;
  requeuedWorkspaceCleanups: number;
};

export type ControlPlaneProviderCompatibilityStatus =
  | "compatible"
  | "incompatible"
  | "unavailable"
  | "local-only"
  | "disabled";

export type ControlPlaneWorkerProviderManifest = {
  provider: ProviderHostProviderKind;
  supportTier: ProviderSupportTier;
  compatibilityStatus: ControlPlaneProviderCompatibilityStatus;
  runtime: ProviderRuntimeDescriptor;
  releasePolicy: ProviderReleasePolicy;
  incompatibilityCode?: string;
  incompatibilityMessage?: string;
  capabilities: ProviderCapabilityMap;
};

export type ControlPlaneWorkerManifest = {
  executionTargetId: string;
  manifestId: string;
  workerStatusCounts: {
    online: number;
    draining: number;
    offline: number;
  };
  lastHeartbeatAt: string;
  workerBuild: {
    version: string;
    gitSha?: string;
    imageDigest?: string;
    operatingSystem: string;
    architecture: string;
  };
  workerProtocol: {
    minimum: number;
    maximum: number;
  };
  runtimeEvent: {
    minimum: number;
    maximum: number;
  };
  processContainment: {
    mode: "none" | "cgroup-v2" | "job-object";
    trustState: "none" | "untrusted" | "verified";
    reasonCode?: "no-attestation" | "legacy-unverified" | "target-policy-mismatch";
  };
  providers: ReadonlyArray<ControlPlaneWorkerProviderManifest>;
};

export type ControlPlaneWorkerReleaseRevision = {
  id: string;
  tenantId: string;
  executionTargetId: string;
  revision: number;
  workerManifestId: string;
  workerBuildVersion: string;
  workerBuildGitSha?: string;
  imageDigest?: string;
  gvisorCompatibleProviders: ReadonlyArray<ProviderHostProviderKind>;
  description: string;
  createdBy: string;
  createdAt: string;
};

export type ControlPlaneWorkerReleasePolicy = {
  tenantId: string;
  executionTargetId: string;
  policyVersion: number;
  promotedRevisionId: string;
  canaryRevisionId?: string;
  canaryPercent: number;
  updatedBy: string;
  updatedAt: string;
};

export type ControlPlaneWorkerReleaseTransition = {
  id: string;
  tenantId: string;
  executionTargetId: string;
  policyVersion: number;
  action: "promote" | "canary" | "rollback" | "abort-canary";
  fromPromotedRevisionId?: string;
  fromCanaryRevisionId?: string;
  toPromotedRevisionId: string;
  toCanaryRevisionId?: string;
  canaryPercent: number;
  reason: string;
  actorId: string;
  requestId?: string;
  occurredAt: string;
};

export type ControlPlaneWorkerReleaseOverview = {
  policy: ControlPlaneWorkerReleasePolicy | null;
  revisions: ReadonlyArray<ControlPlaneWorkerReleaseRevision>;
  transitions: ReadonlyArray<ControlPlaneWorkerReleaseTransition>;
};

export type ControlPlaneSSHProvisionResult = {
  targetId: string;
  operation: "install" | "upgrade" | "revoke";
  status: "active" | "offline" | "disabled";
  serviceName: string;
  binarySha256?: string;
};

export type ControlPlaneArtifactKind =
  | "attachment"
  | "generated_file"
  | "terminal_log"
  | "diff"
  | "workspace_snapshot"
  | "checkpoint";

export type ControlPlaneArtifact = {
  id: string;
  tenantId: string;
  organizationId: string;
  projectId: string;
  sessionId: string;
  executionId: string | null;
  kind: ControlPlaneArtifactKind;
  status: "pending" | "ready" | "deleting" | "deleted" | "failed";
  originalName: string | null;
  contentType: string | null;
  sizeBytes: number | null;
  sha256: string | null;
  createdByType: "user" | "service_account" | "worker" | "system";
  createdById: string;
  readyAt: string | null;
  createdAt: string;
  expiresAt: string | null;
  deletedAt: string | null;
};

export type ControlPlaneArtifactUploadGrant = {
  artifact: ControlPlaneArtifact;
  method: "PUT";
  url: string;
  headers: Readonly<Record<string, string>>;
  expiresAt: string;
};

export type ControlPlaneArtifactDownloadGrant = {
  artifact: ControlPlaneArtifact;
  url: string;
  expiresAt: string;
};

export type ControlPlaneAgentTurn = {
  id: string;
  tenantId: string;
  sessionId: string;
  createdBy: string;
  status: "queued" | "running" | "completed" | "failed" | "cancelled" | "interrupted";
  inputText: string;
  turnKind?: "message" | "compact" | "review" | "rollback" | "fork";
  runtimeMode: RuntimeMode;
  interactionMode: ProviderInteractionMode;
  startedAt: string | null;
  completedAt: string | null;
  createdAt: string;
};

export type ControlPlaneExecutionResume = {
  id: string;
  sessionId: string;
  turnId: string;
  status: "recovering";
  generation: number;
};

export type ControlPlaneRuntimeIsolationDecision = {
  generation: number;
  executionTargetId: string;
  allocationBackend: string;
  requestedRuntime: "auto" | "runc" | "gvisor" | "firecracker";
  requestedProfile: ControlPlaneIsolationProfile;
  effectiveRuntime: "runc" | "gvisor" | "firecracker" | null;
  effectiveProfile: ControlPlaneIsolationProfile | null;
  policySource: "legacy-native" | "target-explicit" | "target-auto" | "lane-policy";
  decision: "selected" | "fallback" | "rejected";
  decisionReasonCode: string | null;
  runtimeClassName: string | null;
  attestedAt: string | null;
  attestationExpiresAt: string | null;
  createdAt: string;
};

export type ControlPlaneReviewTarget =
  | { type: "uncommittedChanges" }
  | { type: "baseBranch"; branch?: string };

export type ControlPlaneControlCommand = {
  id: string;
  executionId: string;
  sessionId: string;
  turnId: string;
  provider: string;
  commandType: string;
  commandId: string;
  payload: Record<string, unknown>;
  status: "pending" | "delivered" | "acknowledged" | "superseded" | "outcome_unknown";
  requestedBy: string;
  requestedAt: string;
  deliveryWorkerId?: string;
  deliveryGeneration?: number;
  deliveryAttempts: number;
  deliveryAvailableAt: string;
  deliveredAt?: string;
  acknowledgedAt?: string;
  deliveryError?: string;
};

export type ControlPlaneAdvancedCommandResult = {
  type: "compact" | "review";
  turn: ControlPlaneAgentTurn;
  executionId: string;
  controlCommand: ControlPlaneControlCommand;
};

export type ControlPlaneRollbackResult = {
  sessionId: string;
  eventId: string;
  eventSequence: number;
  fromSessionId: string;
  fromTurnId: string;
  fromSequence: number;
  removedTurnCount: number;
  supportMode: "emulated";
  workspaceDisposition: "unchanged";
  externalSideEffectsReverted: false;
};

export type ControlPlaneForkResult = {
  session: ControlPlaneAgentSession;
  sourceSessionId: string;
  sourceEventSequence: number;
  supportMode: "emulated";
};

export type ControlPlanePendingInteraction = {
  id: string;
  executionId: string;
  turnId: string;
  provider: string;
  requestId: string;
  kind: "approval" | "user-input";
  payload: Record<string, unknown>;
  requestedAt: string;
  expiresAt: string;
};

export type ControlPlanePendingInteractionSnapshot = {
  items: ReadonlyArray<ControlPlanePendingInteraction>;
  snapshotSequence: number;
};

export type ControlPlaneInteractionResolution = {
  id: string;
  executionId: string;
  sessionId: string;
  requestId: string;
  kind: "approval" | "user-input";
  status: "resolved" | "expired";
};

export type ControlPlaneSessionEvent = {
  eventId: string;
  eventVersion: number;
  tenantId: string;
  organizationId: string;
  projectId: string;
  sessionId: string;
  executionId: string | null;
  workerId: string | null;
  generation: number | null;
  sequence: number;
  eventType: string;
  actorType: "user" | "service_account" | "worker" | "system";
  actorId: string | null;
  payload: Record<string, unknown>;
  occurredAt: string;
};

export type ControlPlaneSessionEventPage = {
  items: ReadonlyArray<ControlPlaneSessionEvent>;
  lastSequence: number;
};

export type ControlPlaneIdempotencyOptions = {
  idempotencyKey: string;
};

export type TenantInvitation = {
  id: string;
  tenantId: string;
  email: string;
  role: string;
  token?: string;
  expiresAt: string;
  createdAt: string;
};

type ErrorEnvelope = {
  error?: {
    code?: string;
    message?: string;
    requestId?: string;
    details?: Record<string, unknown> | null;
  };
};

function normalizeSessionEventSequence(sequence: number): number {
  return Number.isSafeInteger(sequence) && sequence > 0 ? sequence : 0;
}

export function resolveSessionEventStreamUrl(sessionId: string, afterSequence: number): string {
  const query = new URLSearchParams({
    afterSequence: String(normalizeSessionEventSequence(afterSequence)),
  });
  return resolveControlPlaneHttpUrl(
    `/v1/sessions/${encodeURIComponent(sessionId)}/events/stream?${query.toString()}`,
  );
}

function subscribeSessionEvents(
  sessionId: string,
  afterSequence: number,
  handlers: {
    onEvent: (event: ControlPlaneSessionEvent) => void;
    onOpen?: () => void;
    onError?: () => void;
  },
): () => void {
  const reconnectDelayMs = 2_000;
  let closed = false;
  let source: EventSource | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  let lastSequence = normalizeSessionEventSequence(afterSequence);

  const scheduleReconnect = () => {
    if (closed || reconnectTimer !== null) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      connect();
    }, reconnectDelayMs);
  };

  const fail = (failedSource: EventSource) => {
    if (closed || source !== failedSource) return;
    failedSource.close();
    source = null;
    try {
      handlers.onError?.();
    } finally {
      scheduleReconnect();
    }
  };

  const connect = () => {
    if (closed || source !== null) return;
    const nextSource = new EventSource(resolveSessionEventStreamUrl(sessionId, lastSequence), {
      withCredentials: true,
    });
    source = nextSource;
    nextSource.onopen = () => {
      if (!closed && source === nextSource) handlers.onOpen?.();
    };
    nextSource.onerror = () => fail(nextSource);
    nextSource.addEventListener("session-event", (message) => {
      if (closed || source !== nextSource) return;
      try {
        const event = JSON.parse(message.data) as ControlPlaneSessionEvent;
        if (!Number.isSafeInteger(event.sequence) || event.sequence < 1) {
          throw new Error("Session Event sequence must be a positive safe integer.");
        }
        if (event.sequence <= lastSequence) return;
        if (event.sequence !== lastSequence + 1) {
          fail(nextSource);
          return;
        }
        handlers.onEvent(event);
        lastSequence = event.sequence;
      } catch {
        fail(nextSource);
      }
    });
  };

  connect();
  return () => {
    closed = true;
    if (reconnectTimer !== null) clearTimeout(reconnectTimer);
    reconnectTimer = null;
    source?.close();
    source = null;
  };
}

function resolveArtifactGrantUrl(url: string): string {
  return /^https?:\/\//i.test(url) ? url : resolveControlPlaneHttpUrl(url);
}

function auditLogSearchParams(
  filters: ControlPlaneAuditLogFilters,
  page?: { limit?: number; cursor?: string },
): URLSearchParams {
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(filters)) {
    if (typeof value === "string" && value.trim() !== "") query.set(name, value.trim());
  }
  if (page?.limit !== undefined) query.set("limit", String(page.limit));
  if (page?.cursor) query.set("cursor", page.cursor);
  return query;
}

export function resolveAuditLogExportUrl(
  tenantId: string,
  format: "jsonl" | "csv",
  filters: ControlPlaneAuditLogFilters = {},
): string {
  const query = auditLogSearchParams(filters);
  query.set("format", format);
  return resolveControlPlaneHttpUrl(
    `/v1/tenants/${encodeURIComponent(tenantId)}/audit-logs/export?${query.toString()}`,
  );
}

export function resolveInternalCostAllocationExportUrl(tenantId: string): string {
  return resolveControlPlaneHttpUrl(
    `/v1/tenants/${encodeURIComponent(tenantId)}/cost-accounting/export.csv`,
  );
}

async function uploadArtifactPayload(
  grant: ControlPlaneArtifactUploadGrant,
  payload: Blob | ArrayBuffer | ArrayBufferView,
  contentType: string,
): Promise<void> {
  const headers = new Headers(grant.headers);
  headers.set("Content-Type", contentType);
  const response = await fetch(resolveArtifactGrantUrl(grant.url), {
    method: grant.method,
    headers,
    body: payload as BodyInit,
  });
  if (!response.ok) {
    throw new ControlPlaneError(
      response.status,
      "artifact_upload_failed",
      `Artifact upload failed (${response.status}).`,
    );
  }
}

async function controlPlaneRequest<T>(
  path: string,
  init: Omit<RequestInit, "body"> & { body?: unknown } = {},
): Promise<T> {
  const { body: inputBody, ...requestInit } = init;
  const headers = new Headers(requestInit.headers);
  let body: BodyInit | undefined;
  if (inputBody !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(inputBody);
  }
  const response = await fetch(resolveControlPlaneHttpUrl(path), {
    ...requestInit,
    headers,
    ...(body === undefined ? {} : { body }),
    credentials: "include",
  });
  if (!response.ok) {
    const payload = (await response.json().catch(() => null)) as ErrorEnvelope | null;
    throw new ControlPlaneError(
      response.status,
      payload?.error?.code ?? "control_plane_request_failed",
      payload?.error?.message ?? `Control-plane request failed (${response.status}).`,
      payload?.error?.requestId,
      payload?.error?.details ?? undefined,
    );
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

async function controlPlaneRequestWithRawJson<T>(
  path: string,
  init: Omit<RequestInit, "body"> & { body?: unknown } = {},
): Promise<{ data: T; body: string; headers: Headers }> {
  const { body: inputBody, ...requestInit } = init;
  const headers = new Headers(requestInit.headers);
  let body: BodyInit | undefined;
  if (inputBody !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(inputBody);
  }
  const response = await fetch(resolveControlPlaneHttpUrl(path), {
    ...requestInit,
    headers,
    ...(body === undefined ? {} : { body }),
    credentials: "include",
  });
  if (!response.ok) {
    const payload = (await response.json().catch(() => null)) as ErrorEnvelope | null;
    throw new ControlPlaneError(
      response.status,
      payload?.error?.code ?? "control_plane_request_failed",
      payload?.error?.message ?? `Control-plane request failed (${response.status}).`,
      payload?.error?.requestId,
      payload?.error?.details ?? undefined,
    );
  }
  const responseBody = await response.text();
  if (response.status === 204) {
    return { data: undefined as T, body: responseBody, headers: response.headers };
  }
  return { data: JSON.parse(responseBody) as T, body: responseBody, headers: response.headers };
}

async function sha256Hex(value: string): Promise<string> {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) {
    throw new Error("Web Crypto SHA-256 is unavailable.");
  }
  const digest = new Uint8Array(await subtle.digest("SHA-256", new TextEncoder().encode(value)));
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function idempotencyRequestHeaders(
  options?: ControlPlaneIdempotencyOptions,
): { headers: HeadersInit } | Record<never, never> {
  return options ? { headers: { "Idempotency-Key": options.idempotencyKey } } : {};
}

export const controlPlaneClient = {
  getPlatformProfile: () =>
    controlPlaneRequest<ControlPlanePlatformProfile>("/v1/platform/profile"),
  getSession: () => controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/session"),
  devLogin: (input: { email: string; displayName: string }) =>
    controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/dev-login", {
      method: "POST",
      body: input,
    }),
  listPublicIdentityConnections: (tenantSlug: string) => {
    const query = new URLSearchParams({ tenantSlug });
    return controlPlaneRequest<{ items: ReadonlyArray<ControlPlanePublicIdentityConnection> }>(
      `/v1/auth/sso/connections?${query.toString()}`,
    );
  },
  startSSO: (connectionId: string, returnTo = "/settings?section=tenancy") => {
    const query = new URLSearchParams({ returnTo });
    return controlPlaneRequest<{ authorizationUrl: string }>(
      `/v1/auth/sso/${encodeURIComponent(connectionId)}/start?${query.toString()}`,
    );
  },
  startPlatformAdminSSO: (connectionId: string) => {
    const query = new URLSearchParams({ returnTo: "/__synara/platform-admin" });
    return controlPlaneRequest<{ authorizationUrl: string }>(
      `/v1/auth/sso/${encodeURIComponent(connectionId)}/start?${query.toString()}`,
    );
  },
  logout: () => controlPlaneRequest<void>("/v1/auth/logout", { method: "POST" }),
  setActiveTenant: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneSessionState>("/v1/auth/active-tenant", {
      method: "PUT",
      body: { tenantId },
    }),
  createTenant: (input: ControlPlaneCreateTenantInput) =>
    controlPlaneRequest<ControlPlaneTenantAccess>("/v1/tenants", {
      method: "POST",
      body: { ...input, entitlementProfileCode: "standard", status: "active" },
    }),
  provisionPlatformTenant: (input: ControlPlaneProvisionTenantInput) =>
    controlPlaneRequest<ControlPlaneProvisionedTenant>("/v1/platform/tenants", {
      method: "POST",
      body: input,
    }),
  getPlatformTenantEntitlements: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneEntitlementSnapshot>(
      `/v1/platform/tenants/${encodeURIComponent(tenantId)}/entitlements`,
    ),
  assignPlatformTenantEntitlementProfile: (
    tenantId: string,
    input: ControlPlaneAssignTenantEntitlementProfileInput,
  ) =>
    controlPlaneRequest<ControlPlaneEntitlementSnapshot>(
      `/v1/platform/tenants/${encodeURIComponent(tenantId)}/entitlement-profile`,
      { method: "PUT", body: input },
    ),
  listPlatformStage6ReleaseCandidates: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6ReleaseCandidate> }>(
      "/v1/platform/release-candidates",
    ),
  createPlatformStage6ReleaseCandidate: (input: ControlPlaneCreateStage6ReleaseCandidateInput) =>
    controlPlaneRequest<ControlPlaneStage6ReleaseCandidate>("/v1/platform/release-candidates", {
      method: "POST",
      body: input,
    }),
  getPlatformStage6ReleaseReadiness: (candidateRecordId: string) =>
    controlPlaneRequest<ControlPlaneStage6ReleaseReadiness>(
      `/v1/platform/release-candidates/${encodeURIComponent(candidateRecordId)}/readiness`,
    ),
  recordPlatformStage6ReleaseApproval: (
    candidateRecordId: string,
    input: ControlPlaneRecordStage6ReleaseApprovalInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ReleaseCandidate>(
      `/v1/platform/release-candidates/${encodeURIComponent(candidateRecordId)}/approvals`,
      { method: "POST", body: input },
    ),
  recordPlatformStage6ReleaseFinalReview: (
    candidateRecordId: string,
    input: ControlPlaneRecordStage6ReleaseFinalReviewInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ReleaseCandidate>(
      `/v1/platform/release-candidates/${encodeURIComponent(candidateRecordId)}/final-review`,
      { method: "POST", body: input },
    ),
  transitionPlatformStage6ReleaseCandidate: (
    candidateRecordId: string,
    input: ControlPlaneTransitionStage6ReleaseCandidateInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ReleaseCandidate>(
      `/v1/platform/release-candidates/${encodeURIComponent(candidateRecordId)}/transitions`,
      { method: "POST", body: input },
    ),
  listPlatformStage6Incidents: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6Incident> }>(
      "/v1/platform/incidents",
    ),
  createPlatformStage6Incident: (input: ControlPlaneCreateStage6IncidentInput) =>
    controlPlaneRequest<ControlPlaneStage6Incident>("/v1/platform/incidents", {
      method: "POST",
      body: input,
    }),
  bindPlatformStage6IncidentStatusBoard: (
    incidentId: string,
    input: {
      expectedVersion: number;
      internalStatusBoardIncidentReference: string;
      reason: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6Incident>(
      `/v1/platform/incidents/${encodeURIComponent(incidentId)}/status-board`,
      { method: "POST", body: input },
    ),
  addPlatformStage6IncidentInternalUpdate: (
    incidentId: string,
    input: ControlPlaneAddStage6IncidentInternalUpdateInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6Incident>(
      `/v1/platform/incidents/${encodeURIComponent(incidentId)}/internal-updates`,
      { method: "POST", body: input },
    ),
  recordPlatformStage6IncidentResolutionApproval: (
    incidentId: string,
    input: {
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6Incident>(
      `/v1/platform/incidents/${encodeURIComponent(incidentId)}/resolution-approval`,
      { method: "POST", body: input },
    ),
  transitionPlatformStage6Incident: (
    incidentId: string,
    input: {
      expectedVersion: number;
      targetState: Exclude<ControlPlaneStage6IncidentState, "investigating">;
      reason: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6Incident>(
      `/v1/platform/incidents/${encodeURIComponent(incidentId)}/transitions`,
      { method: "POST", body: input },
    ),
  listPlatformStage6SLOWindows: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6SLOWindow> }>(
      "/v1/platform/slo-windows",
    ),
  importPlatformStage6SLOWindow: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6SLOWindow>("/v1/platform/slo-windows", {
      method: "POST",
      body: input,
    }),
  recordPlatformStage6SLOApproval: (
    sloWindowRecordId: string,
    input: {
      role: ControlPlaneStage6SLOApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6SLOWindow>(
      `/v1/platform/slo-windows/${encodeURIComponent(sloWindowRecordId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6RecoveryDrills: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6RecoveryDrill> }>(
      "/v1/platform/recovery-drills",
    ),
  importPlatformStage6RecoveryDrill: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6RecoveryDrill>("/v1/platform/recovery-drills", {
      method: "POST",
      body: input,
    }),
  recordPlatformStage6RecoveryApproval: (
    recoveryDrillRecordId: string,
    input: {
      role: ControlPlaneStage6RecoveryApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6RecoveryDrill>(
      `/v1/platform/recovery-drills/${encodeURIComponent(recoveryDrillRecordId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6PenetrationEngagements: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6PenetrationEngagement> }>(
      "/v1/platform/penetration-engagements",
    ),
  importPlatformStage6PenetrationEngagement: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6PenetrationEngagement>(
      "/v1/platform/penetration-engagements",
      { method: "POST", body: input },
    ),
  recordPlatformStage6PenetrationApproval: (
    penetrationEngagementId: string,
    input: {
      role: ControlPlaneStage6PenetrationApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6PenetrationEngagement>(
      `/v1/platform/penetration-engagements/${encodeURIComponent(penetrationEngagementId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6CapacityRuns: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6CapacityRun> }>(
      "/v1/platform/capacity-runs",
    ),
  importPlatformStage6CapacityRun: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6CapacityRun>("/v1/platform/capacity-runs", {
      method: "POST",
      body: input,
    }),
  recordPlatformStage6CapacityApproval: (
    capacityRunId: string,
    input: {
      role: ControlPlaneStage6CapacityApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6CapacityRun>(
      `/v1/platform/capacity-runs/${encodeURIComponent(capacityRunId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6IncidentExercises: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6IncidentExercise> }>(
      "/v1/platform/incident-exercises",
    ),
  importPlatformStage6IncidentExercise: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6IncidentExercise>("/v1/platform/incident-exercises", {
      method: "POST",
      body: input,
    }),
  recordPlatformStage6IncidentExerciseApproval: (
    incidentExerciseId: string,
    input: {
      role: ControlPlaneStage6IncidentExerciseApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6IncidentExercise>(
      `/v1/platform/incident-exercises/${encodeURIComponent(incidentExerciseId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6OperationsExercises: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6OperationsExercise> }>(
      "/v1/platform/operations-exercises",
    ),
  importPlatformStage6OperationsExercise: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6OperationsExercise>("/v1/platform/operations-exercises", {
      method: "POST",
      body: input,
    }),
  recordPlatformStage6OperationsExerciseApproval: (
    operationsExerciseId: string,
    input: {
      role: ControlPlaneStage6OperationsExerciseApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6OperationsExercise>(
      `/v1/platform/operations-exercises/${encodeURIComponent(operationsExerciseId)}/approvals`,
      { method: "POST", body: input },
    ),
  listPlatformStage6CompliancePrograms: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6ComplianceProgram> }>(
      "/v1/platform/compliance-programs",
    ),
  createPlatformStage6ComplianceProgram: (input: ControlPlaneCreateStage6ComplianceProgramInput) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>("/v1/platform/compliance-programs", {
      method: "POST",
      body: input,
    }),
  listPlatformStage6InternalCostReviews: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneStage6InternalCostReview> }>(
      "/v1/platform/internal-cost-reviews",
    ),
  importPlatformStage6InternalCostReview: (input: {
    candidateRecordId: string;
    receiptBase64: string;
    receiptSha256: string;
  }) =>
    controlPlaneRequest<ControlPlaneStage6InternalCostReview>(
      "/v1/platform/internal-cost-reviews",
      { method: "POST", body: input },
    ),
  recordPlatformStage6InternalCostApproval: (
    reviewId: string,
    input: {
      role: ControlPlaneStage6InternalCostApprovalRole;
      decision: "approved" | "rejected";
      reason: string;
      evidenceReference: string;
      evidenceSha256: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneStage6InternalCostReview>(
      `/v1/platform/internal-cost-reviews/${encodeURIComponent(reviewId)}/approvals`,
      { method: "POST", body: input },
    ),
  createPlatformStage6ComplianceControl: (
    programId: string,
    input: ControlPlaneCreateStage6ComplianceControlInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>(
      `/v1/platform/compliance-programs/${encodeURIComponent(programId)}/controls`,
      { method: "POST", body: input },
    ),
  submitPlatformStage6ComplianceEvidence: (
    programId: string,
    input: ControlPlaneSubmitStage6ComplianceEvidenceInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>(
      `/v1/platform/compliance-programs/${encodeURIComponent(programId)}/evidence`,
      { method: "POST", body: input },
    ),
  reviewPlatformStage6ComplianceEvidence: (
    programId: string,
    evidenceRecordId: string,
    input: ControlPlaneReviewStage6ComplianceEvidenceInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>(
      `/v1/platform/compliance-programs/${encodeURIComponent(programId)}/evidence/${encodeURIComponent(evidenceRecordId)}/review`,
      { method: "POST", body: input },
    ),
  recordPlatformStage6ComplianceDecision: (
    programId: string,
    input: ControlPlaneRecordStage6ComplianceDecisionInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>(
      `/v1/platform/compliance-programs/${encodeURIComponent(programId)}/decisions`,
      { method: "POST", body: input },
    ),
  transitionPlatformStage6ComplianceProgram: (
    programId: string,
    input: ControlPlaneTransitionStage6ComplianceProgramInput,
  ) =>
    controlPlaneRequest<ControlPlaneStage6ComplianceProgram>(
      `/v1/platform/compliance-programs/${encodeURIComponent(programId)}/transitions`,
      { method: "POST", body: input },
    ),
  listPlatformProviderCommercialAuthorizations: () =>
    controlPlaneRequest<{
      items: ReadonlyArray<ControlPlaneProviderCommercialAuthorization>;
    }>("/v1/platform/provider-commercial-authorizations"),
  createPlatformProviderCommercialAuthorization: (
    input: ControlPlaneCreateProviderCommercialAuthorizationInput,
  ) =>
    controlPlaneRequest<ControlPlaneProviderCommercialAuthorization>(
      "/v1/platform/provider-commercial-authorizations",
      { method: "POST", body: input },
    ),
  recordPlatformProviderCommercialApproval: (
    authorizationId: string,
    input: ControlPlaneRecordProviderCommercialApprovalInput,
  ) =>
    controlPlaneRequest<ControlPlaneProviderCommercialAuthorization>(
      `/v1/platform/provider-commercial-authorizations/${encodeURIComponent(authorizationId)}/approvals`,
      { method: "POST", body: input },
    ),
  transitionPlatformProviderCommercialAuthorization: (
    authorizationId: string,
    input: ControlPlaneTransitionProviderCommercialAuthorizationInput,
  ) =>
    controlPlaneRequest<ControlPlaneProviderCommercialAuthorization>(
      `/v1/platform/provider-commercial-authorizations/${encodeURIComponent(authorizationId)}/transitions`,
      { method: "POST", body: input },
    ),
  listPlatformGovernanceAuthorities: () =>
    controlPlaneRequest<{
      items: ReadonlyArray<ControlPlaneGovernanceAuthorityGrant>;
      authorityKeys: ReadonlyArray<ControlPlaneGovernanceAuthorityKey>;
      operators: ReadonlyArray<ControlPlaneGovernanceAuthorityOperator>;
    }>("/v1/platform/governance-authorities"),
  createPlatformGovernanceAuthority: (input: ControlPlaneCreateGovernanceAuthorityInput) =>
    controlPlaneRequest<ControlPlaneGovernanceAuthorityGrant>(
      "/v1/platform/governance-authorities",
      { method: "POST", body: input },
    ),
  revokePlatformGovernanceAuthority: (
    grantId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneGovernanceAuthorityGrant>(
      `/v1/platform/governance-authorities/${encodeURIComponent(grantId)}/revoke`,
      { method: "POST", body: input },
    ),
  getPlatformDesktopAccess: (tenantId: string) =>
    controlPlaneRequest<ControlPlanePlatformDesktopAccess>(
      `/v1/platform/tenants/${encodeURIComponent(tenantId)}/desktop-access`,
    ),
  issuePlatformDesktopEnrollment: (
    tenantId: string,
    input: ControlPlaneIssueDesktopEnrollmentInput,
  ) =>
    controlPlaneRequest<ControlPlaneIssuedDesktopEnrollment>(
      `/v1/platform/tenants/${encodeURIComponent(tenantId)}/desktop-enrollments`,
      { method: "POST", body: input },
    ),
  markPlatformDesktopEnrollmentOpened: (enrollmentId: string, expectedVersion: number) =>
    controlPlaneRequest<ControlPlaneDesktopEnrollment>(
      `/v1/platform/desktop-enrollments/${encodeURIComponent(enrollmentId)}/opened`,
      { method: "POST", body: { expectedVersion } },
    ),
  revokePlatformDesktopEnrollment: (
    enrollmentId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneDesktopEnrollment>(
      `/v1/platform/desktop-enrollments/${encodeURIComponent(enrollmentId)}/revoke`,
      { method: "POST", body: input },
    ),
  revokePlatformDesktopDevice: (
    deviceId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneDesktopDevice>(
      `/v1/platform/desktop-devices/${encodeURIComponent(deviceId)}/revoke`,
      { method: "POST", body: input },
    ),
  transitionTenant: (
    tenantId: string,
    input: {
      toStatus: "active" | "suspended" | "closed";
      expectedVersion: number;
      reason: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneTenantAccess>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/lifecycle-transitions`,
      { method: "POST", body: input },
    ),
  requestTenantDeletion: (tenantId: string, input: { expectedVersion: number; reason: string }) =>
    controlPlaneRequest<void>(`/v1/tenants/${encodeURIComponent(tenantId)}/deletion-requests`, {
      method: "POST",
      body: input,
    }),
  listTenantDeletionRequests: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneDeletingTenant> }>(
      "/v1/tenants/deletion-requests",
    ),
  restoreTenant: (tenantId: string, input: { expectedVersion: number; reason: string }) =>
    controlPlaneRequest<ControlPlaneTenantAccess>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/restore`,
      { method: "POST", body: input },
    ),
  getTenantQuota: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantQuota>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/quota`,
    ),
  getTenantEntitlements: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneEntitlementSnapshot>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/entitlements`,
    ),
  getTenantUsage: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantUsage>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/usage`,
    ),
  getTenantUsageExport: async (tenantId: string) => {
    const response = await controlPlaneRequestWithRawJson<ControlPlaneTenantUsage>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/usage/export.json`,
    );
    const schema = response.headers.get("X-Synara-Usage-Export-Schema") ?? "";
    const sha256 = response.headers.get("X-Synara-Usage-Export-SHA256") ?? "";
    const bytesHeader = response.headers.get("X-Synara-Usage-Export-Bytes");
    const bytes = bytesHeader === null ? Number.NaN : Number(bytesHeader);
    const actualBytes = new TextEncoder().encode(response.body).byteLength;
    let actualSHA256 = "";
    try {
      actualSHA256 = await sha256Hex(response.body);
    } catch {
      // Treat an environment without Web Crypto as an integrity failure; do
      // not download an unverified internal accounting artifact.
    }
    if (
      schema !== "json-v1" ||
      !/^[0-9a-f]{64}$/.test(sha256) ||
      sha256 !== actualSHA256 ||
      !Number.isSafeInteger(bytes) ||
      bytes < 1 ||
      bytes !== actualBytes
    ) {
      throw new ControlPlaneError(
        502,
        "tenant_usage_export_integrity_invalid",
        "The Tenant Usage export integrity headers do not match the server response.",
      );
    }
    return { usage: response.data, body: response.body, sha256, bytes };
  },
  getTenantSupportDiagnosticExport: async (tenantId: string) => {
    const response = await controlPlaneRequestWithRawJson<ControlPlaneTenantSupportDiagnostic>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/support-diagnostic.json`,
    );
    const schema = response.headers.get("X-Synara-Support-Diagnostic-Schema") ?? "";
    const sha256 = response.headers.get("X-Synara-Support-Diagnostic-SHA256") ?? "";
    const bytesHeader = response.headers.get("X-Synara-Support-Diagnostic-Bytes");
    const bytes = bytesHeader === null ? Number.NaN : Number(bytesHeader);
    const actualBytes = new TextEncoder().encode(response.body).byteLength;
    let actualSHA256 = "";
    try {
      actualSHA256 = await sha256Hex(response.body);
    } catch {
      // Treat an environment without Web Crypto as an integrity failure; do
      // not download an unverified internal support artifact.
    }
    if (
      schema !== "json-v1" ||
      !/^[0-9a-f]{64}$/.test(sha256) ||
      sha256 !== actualSHA256 ||
      !Number.isSafeInteger(bytes) ||
      bytes < 1 ||
      bytes !== actualBytes
    ) {
      throw new ControlPlaneError(
        502,
        "tenant_support_diagnostic_integrity_invalid",
        "The Tenant Support diagnostic integrity headers do not match the server response.",
      );
    }
    return { diagnostic: response.data, body: response.body, sha256, bytes };
  },
  getInternalCostAllocationReport: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneInternalCostAllocationReport>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/cost-accounting/report`,
    ),
  putProjectCostAllocation: (
    tenantId: string,
    projectId: string,
    input: { costCenterCode: string; departmentCode: string; expectedVersion: number },
  ) =>
    controlPlaneRequest<ControlPlaneProjectCostAllocation>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/cost-accounting/projects/${encodeURIComponent(projectId)}`,
      { method: "PUT", body: input },
    ),
  getTenantExecutionSchedulingPolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneExecutionSchedulingPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-scheduling-policy`,
    ),
  getTenantDataResidencyStatement: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneDataResidencyStatement>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/data-residency-statement`,
    ),
  getTenantDataResidencyStatementDownload: async (tenantId: string) => {
    const response = await controlPlaneRequestWithRawJson<ControlPlaneDataResidencyStatement>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/data-residency-statement`,
    );
    const sha256 = response.headers.get("X-Synara-Data-Residency-SHA256") ?? "";
    const bytesHeader = response.headers.get("X-Synara-Data-Residency-Bytes");
    const bytes = bytesHeader === null ? Number.NaN : Number(bytesHeader);
    const actualBytes = new TextEncoder().encode(response.body).byteLength;
    if (
      !/^[0-9a-f]{64}$/.test(sha256) ||
      !Number.isSafeInteger(bytes) ||
      bytes < 1 ||
      bytes !== actualBytes
    ) {
      throw new ControlPlaneError(
        502,
        "data_residency_statement_integrity_invalid",
        "The data residency statement integrity headers do not match the server response.",
      );
    }
    return { statement: response.data, body: response.body, sha256, bytes };
  },
  updateTenantExecutionSchedulingPolicy: (
    tenantId: string,
    input: { expectedVersion: number; document: ControlPlaneSchedulingPolicyDocument },
  ) =>
    controlPlaneRequest<ControlPlaneExecutionSchedulingPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-scheduling-policy`,
      { method: "PUT", body: input },
    ),
  getTenantSupportPolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantSupportPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/support-policy`,
    ),
  updateTenantSupportPolicy: (
    tenantId: string,
    input: Pick<ControlPlaneTenantSupportPolicy, "supportAccessEnabled" | "version" | "reason">,
  ) =>
    controlPlaneRequest<ControlPlaneTenantSupportPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/support-policy`,
      {
        method: "PUT",
        body: {
          supportAccessEnabled: input.supportAccessEnabled,
          expectedVersion: input.version,
          reason: input.reason,
        },
      },
    ),
  listTenantSupportAccess: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneSupportAccessGrant> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/support-access`,
    ),
  revokeTenantSupportAccess: (
    tenantId: string,
    grantId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneSupportAccessGrant>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/support-access/${encodeURIComponent(grantId)}/revoke`,
      { method: "POST", body: input },
    ),
  listPlatformSupportAccess: () =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneSupportAccessGrant> }>(
      "/v1/platform/support-access",
    ),
  listPlatformTenants: () =>
    controlPlaneRequest<ControlPlanePlatformTenantOverview>("/v1/platform/tenants"),
  requestPlatformSupportAccess: (input: {
    tenantId: string;
    reason: string;
    requestedDurationSeconds: number;
  }) =>
    controlPlaneRequest<ControlPlaneSupportAccessGrant>("/v1/platform/support-access/requests", {
      method: "POST",
      body: input,
    }),
  approvePlatformSupportAccess: (
    grantId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneSupportAccessGrant>(
      `/v1/platform/support-access/${encodeURIComponent(grantId)}/approve`,
      { method: "POST", body: input },
    ),
  denyPlatformSupportAccess: (
    grantId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneSupportAccessGrant>(
      `/v1/platform/support-access/${encodeURIComponent(grantId)}/deny`,
      { method: "POST", body: input },
    ),
  revokePlatformSupportAccess: (
    grantId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneSupportAccessGrant>(
      `/v1/platform/support-access/${encodeURIComponent(grantId)}/revoke`,
      { method: "POST", body: input },
    ),
  updateTenantQuota: (
    tenantId: string,
    input: Pick<ControlPlaneTenantQuota, "maxConcurrentExecutions" | "maxArtifactBytes">,
  ) =>
    controlPlaneRequest<ControlPlaneTenantQuota>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/quota`,
      { method: "PUT", body: input },
    ),
  getRetentionPolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneRetentionPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/retention-policy`,
    ),
  updateRetentionPolicy: (
    tenantId: string,
    input: Pick<ControlPlaneRetentionPolicy, "sessionArchiveAfterDays" | "artifactDeleteAfterDays">,
  ) =>
    controlPlaneRequest<ControlPlaneRetentionPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/retention-policy`,
      { method: "PUT", body: input },
    ),
  listLegalHolds: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneLegalHold> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/legal-holds`,
    ),
  createLegalHold: (
    tenantId: string,
    input: Pick<
      ControlPlaneLegalHold,
      "scopeType" | "scopeId" | "name" | "matterReference" | "reason"
    >,
  ) =>
    controlPlaneRequest<ControlPlaneLegalHold>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/legal-holds`,
      { method: "POST", body: input },
    ),
  releaseLegalHold: (
    tenantId: string,
    holdId: string,
    input: { expectedVersion: number; reason: string },
  ) =>
    controlPlaneRequest<ControlPlaneLegalHold>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/legal-holds/${encodeURIComponent(holdId)}/release`,
      { method: "POST", body: input },
    ),
  listPrivacyRequests: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlanePrivacyRequest> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/privacy-requests`,
    ),
  createPrivacyRequest: (
    tenantId: string,
    input: { subjectUserId?: string; requestType: "access_export" | "erasure"; reason: string },
  ) =>
    controlPlaneRequest<ControlPlanePrivacyRequest>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/privacy-requests`,
      { method: "POST", body: input },
    ),
  transitionPrivacyRequest: (
    tenantId: string,
    privacyRequestId: string,
    input: { expectedVersion: number; toStatus: string; reason: string },
  ) =>
    controlPlaneRequest<ControlPlanePrivacyRequest>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/privacy-requests/${encodeURIComponent(privacyRequestId)}/transitions`,
      { method: "POST", body: input },
    ),
  executePrivacyExport: (tenantId: string, privacyRequestId: string, expectedVersion: number) =>
    controlPlaneRequest<ControlPlanePrivacyExportResult>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/privacy-requests/${encodeURIComponent(privacyRequestId)}/export`,
      { method: "POST", body: { expectedVersion } },
    ),
  executePrivacyErasure: (tenantId: string, privacyRequestId: string, expectedVersion: number) =>
    controlPlaneRequest<{
      request: ControlPlanePrivacyRequest;
      summary: Readonly<Record<string, unknown>>;
    }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/privacy-requests/${encodeURIComponent(privacyRequestId)}/erasure`,
      { method: "POST", body: { expectedVersion } },
    ),
  executeTenantDataExport: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantDataExportResult>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/data-export`,
      { method: "POST" },
    ),
  getTenantResourceLifecyclePolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneResourceLifecyclePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/resource-lifecycle-policy`,
    ),
  updateTenantResourceLifecyclePolicy: (
    tenantId: string,
    input: ControlPlaneResourceLifecyclePolicyUpdateInput,
  ) =>
    controlPlaneRequest<ControlPlaneResourceLifecyclePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/resource-lifecycle-policy`,
      { method: "PUT", body: input },
    ),
  listIdentityConnections: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityConnection> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections`,
    ),
  createIdentityConnection: (
    tenantId: string,
    input: {
      kind: "oidc" | "saml";
      name: string;
      issuer: string;
      clientId?: string;
      clientSecret?: string;
      oidc?: {
        scopes?: ReadonlyArray<string>;
        allowedDomains?: ReadonlyArray<string>;
        groupsClaim?: string;
        defaultTenantRole?: string;
      };
      saml?: {
        metadataUrl?: string;
        entityId?: string;
        emailAttribute?: string;
        displayNameAttribute?: string;
        groupsAttribute?: string;
        allowedDomains?: ReadonlyArray<string>;
        defaultTenantRole?: string;
      };
    },
  ) =>
    controlPlaneRequest<ControlPlaneIdentityConnection>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections`,
      { method: "POST", body: input },
    ),
  disableIdentityConnection: (tenantId: string, connectionId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/disable`,
      { method: "POST" },
    ),
  listIdentityDomains: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityDomain> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-domains`,
    ),
  createIdentityDomain: (tenantId: string, domain: string) =>
    controlPlaneRequest<ControlPlaneIdentityDomainChallenge>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-domains`,
      { method: "POST", body: { domain } },
    ),
  verifyIdentityDomain: (tenantId: string, domainId: string) =>
    controlPlaneRequest<ControlPlaneIdentityDomain>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-domains/${encodeURIComponent(domainId)}/verify`,
      { method: "POST" },
    ),
  revokeIdentityDomain: (tenantId: string, domainId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-domains/${encodeURIComponent(domainId)}/revoke`,
      { method: "POST" },
    ),
  getTenantIdentityPolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneTenantIdentityPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-policy`,
    ),
  updateTenantIdentityPolicy: (
    tenantId: string,
    input: Pick<ControlPlaneTenantIdentityPolicy, "ssoEnforcement" | "recoveryUserId"> & {
      expectedVersion: number;
    },
  ) =>
    controlPlaneRequest<ControlPlaneTenantIdentityPolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-policy`,
      { method: "PUT", body: input },
    ),
  listIdentityGroupMappings: (tenantId: string, connectionId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityGroupMapping> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/group-mappings`,
    ),
  replaceIdentityGroupMappings: (
    tenantId: string,
    connectionId: string,
    items: ReadonlyArray<Omit<ControlPlaneIdentityGroupMapping, "id">>,
  ) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneIdentityGroupMapping> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/identity-connections/${encodeURIComponent(connectionId)}/group-mappings`,
      { method: "PUT", body: { items } },
    ),
  listServiceAccounts: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneServiceAccount> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts`,
    ),
  createServiceAccount: (
    tenantId: string,
    input: {
      organizationId?: string;
      name: string;
      description: string;
      role?: ControlPlaneServiceAccount["role"];
      scopes: ReadonlyArray<string>;
      rateLimitPerMinute?: number;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneIssuedServiceAccount>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts`,
      { method: "POST", body: input },
    ),
  getServiceAccountAPIUsage: (
    tenantId: string,
    serviceAccountId: string,
    range: { from?: string; to?: string } = {},
  ) => {
    const query = new URLSearchParams();
    if (range.from) query.set("from", range.from);
    if (range.to) query.set("to", range.to);
    const suffix = query.size > 0 ? `?${query.toString()}` : "";
    return controlPlaneRequest<ControlPlaneServiceAccountUsageReport>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts/${encodeURIComponent(serviceAccountId)}/usage${suffix}`,
    );
  },
  rotateServiceAccountToken: (tenantId: string, serviceAccountId: string) =>
    controlPlaneRequest<{ token: string; expiresAt: string | null }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts/${encodeURIComponent(serviceAccountId)}/rotate-token`,
      { method: "POST", body: { expiresAt: null } },
    ),
  revokeServiceAccount: (tenantId: string, serviceAccountId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/service-accounts/${encodeURIComponent(serviceAccountId)}/revoke`,
      { method: "POST" },
    ),
  listDeveloperWebhooks: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneDeveloperWebhook> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks`,
    ),
  createDeveloperWebhook: (
    tenantId: string,
    input: {
      name: string;
      url: string;
      eventTypes: ReadonlyArray<ControlPlaneDeveloperWebhookEventType>;
    },
  ) =>
    controlPlaneRequest<ControlPlaneIssuedDeveloperWebhook>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks`,
      { method: "POST", body: input },
    ),
  rotateDeveloperWebhookSecret: (tenantId: string, webhookId: string) =>
    controlPlaneRequest<ControlPlaneIssuedDeveloperWebhook>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks/${encodeURIComponent(webhookId)}/rotate-secret`,
      { method: "POST" },
    ),
  setDeveloperWebhookEnabled: (tenantId: string, webhookId: string, enabled: boolean) =>
    controlPlaneRequest<ControlPlaneDeveloperWebhook>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks/${encodeURIComponent(webhookId)}/${enabled ? "enable" : "disable"}`,
      { method: "POST" },
    ),
  revokeDeveloperWebhook: (tenantId: string, webhookId: string) =>
    controlPlaneRequest<ControlPlaneDeveloperWebhook>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks/${encodeURIComponent(webhookId)}/revoke`,
      { method: "POST" },
    ),
  listDeveloperWebhookDeliveries: (tenantId: string, webhookId: string, limit = 50) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneDeveloperWebhookDelivery> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks/${encodeURIComponent(webhookId)}/deliveries?limit=${encodeURIComponent(String(limit))}`,
    ),
  replayDeveloperWebhookDelivery: (tenantId: string, webhookId: string, deliveryId: string) =>
    controlPlaneRequest<unknown>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/developer-webhooks/${encodeURIComponent(webhookId)}/deliveries/${encodeURIComponent(deliveryId)}/replay`,
      { method: "POST" },
    ),
  listCredentials: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneCredential> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials`,
    ),
  createCredential: (
    tenantId: string,
    input: {
      organizationId?: string;
      scope?: ControlPlaneCredentialScope;
      scopeUserId?: string;
      selectorOrganizationId?: string;
      selectorModel?: string;
      autoSelectEnabled?: boolean;
      name: string;
      purpose: ControlPlaneCredentialPurpose;
      provider: string;
      credentialType: string;
      payload: Record<string, unknown>;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials`,
      { method: "POST", body: input },
    ),
  rotateCredential: (
    tenantId: string,
    credentialId: string,
    input: {
      expectedVersion: number;
      payload: Record<string, unknown>;
      expiresAt: string | null;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/rotate`,
      { method: "POST", body: input },
    ),
  revokeCredential: (tenantId: string, credentialId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/revoke`,
      { method: "POST" },
    ),
  setCredentialAutoSelect: (tenantId: string, credentialId: string, enabled: boolean) =>
    controlPlaneRequest<ControlPlaneCredential>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credentials/${encodeURIComponent(credentialId)}/auto-select`,
      { method: "PUT", body: { enabled } },
    ),
  getProviderCredentialScopePolicy: (tenantId: string) =>
    controlPlaneRequest<ControlPlaneProviderCredentialScopePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/provider-credential-scope-policy`,
    ),
  updateProviderCredentialScopePolicy: (
    tenantId: string,
    input: {
      platformCredentialsEnabled: boolean;
      platformCredentialAutoSelect: boolean;
    },
  ) =>
    controlPlaneRequest<ControlPlaneProviderCredentialScopePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/provider-credential-scope-policy`,
      { method: "PUT", body: input },
    ),
  listCredentialBindings: (
    tenantId: string,
    owner: { projectId: string } | { executionTargetId: string },
  ) => {
    const query = new URLSearchParams(owner).toString();
    return controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneCredentialBinding> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings?${query}`,
    );
  },
  createCredentialBinding: (
    tenantId: string,
    input: {
      projectId?: string;
      executionTargetId?: string;
      credentialId: string;
      bindingKind: ControlPlaneCredentialBindingKind;
      selector?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneCredentialBinding>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings`,
      { method: "POST", body: input },
    ),
  disableCredentialBinding: (tenantId: string, bindingId: string) =>
    controlPlaneRequest<ControlPlaneCredentialBinding>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/credential-bindings/${encodeURIComponent(bindingId)}/disable`,
      { method: "POST" },
    ),
  listAuditLogs: (
    tenantId: string,
    filters: ControlPlaneAuditLogFilters = {},
    page: { limit?: number; cursor?: string } = {},
  ) => {
    const query = auditLogSearchParams(filters, page);
    const suffix = query.size > 0 ? `?${query.toString()}` : "";
    return controlPlaneRequest<ControlPlaneAuditLogPage>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/audit-logs${suffix}`,
    );
  },
  listOrganizations: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneOrganization> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations`,
    ),
  createOrganization: (
    tenantId: string,
    input: { slug: string; name: string; kind: "team" | "department" | "personal" },
  ) =>
    controlPlaneRequest<ControlPlaneOrganization>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations`,
      { method: "POST", body: { ...input, settings: {} } },
    ),
  listProjects: (
    tenantId: string,
    organizationId: string,
    page?: { limit?: number; cursor?: string },
  ) => {
    const query = new URLSearchParams();
    if (page?.limit !== undefined) query.set("limit", String(page.limit));
    if (page?.cursor) query.set("cursor", page.cursor);
    const suffix = query.size > 0 ? `?${query.toString()}` : "";
    return controlPlaneRequest<{
      items: ReadonlyArray<ControlPlaneProject>;
      nextCursor?: string;
    }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations/${encodeURIComponent(organizationId)}/projects${suffix}`,
    );
  },
  createProject: (
    tenantId: string,
    organizationId: string,
    input: {
      name: string;
      repositoryUrl?: string;
      defaultBranch: string;
      visibility: ControlPlaneProject["visibility"];
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneProject>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/organizations/${encodeURIComponent(organizationId)}/projects`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  updateProject: (
    projectId: string,
    input: {
      name?: string;
      repositoryUrl?: string;
      defaultBranch?: string;
      visibility?: ControlPlaneProject["visibility"];
    },
  ) =>
    controlPlaneRequest<ControlPlaneProject>(`/v1/projects/${encodeURIComponent(projectId)}`, {
      method: "PATCH",
      body: input,
    }),
  listProjectSessions: (projectId: string, page?: { limit?: number; cursor?: string }) => {
    const query = new URLSearchParams();
    if (page?.limit !== undefined) query.set("limit", String(page.limit));
    if (page?.cursor) query.set("cursor", page.cursor);
    const encodedQuery = query.toString();
    const suffix = encodedQuery ? `?${encodedQuery}` : "";
    return controlPlaneRequest<{
      items: ReadonlyArray<ControlPlaneAgentSession>;
      nextCursor: string | null;
    }>(`/v1/projects/${encodeURIComponent(projectId)}/sessions${suffix}`);
  },
  getProjectResourceLifecyclePolicy: (projectId: string) =>
    controlPlaneRequest<ControlPlaneResourceLifecyclePolicy>(
      `/v1/projects/${encodeURIComponent(projectId)}/resource-lifecycle-policy`,
    ),
  updateProjectResourceLifecyclePolicy: (
    projectId: string,
    input: ControlPlaneResourceLifecyclePolicyUpdateInput,
  ) =>
    controlPlaneRequest<ControlPlaneResourceLifecyclePolicy>(
      `/v1/projects/${encodeURIComponent(projectId)}/resource-lifecycle-policy`,
      { method: "PUT", body: input },
    ),
  getProjectProviderCapabilities: (projectId: string, executionTargetId?: string) => {
    const query = executionTargetId
      ? `?${new URLSearchParams({ executionTargetId }).toString()}`
      : "";
    return controlPlaneRequest<ProviderCapabilityProjection>(
      `/v1/projects/${encodeURIComponent(projectId)}/provider-capabilities${query}`,
    );
  },
  createSession: (
    projectId: string,
    input: {
      title: string;
      visibility: ControlPlaneAgentSession["visibility"];
      provider: ProviderKind;
      model?: string;
      providerCredentialId?: string;
      executionTargetId?: string;
      executionTargetGroupId?: string;
      preferredExecutionRegion?: string;
      resourceLifecyclePolicy?: ControlPlaneResourceLifecycleOverrideInput;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAgentSession>(
      `/v1/projects/${encodeURIComponent(projectId)}/sessions`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  getAgentSession: (sessionId: string) =>
    controlPlaneRequest<ControlPlaneAgentSession>(`/v1/sessions/${encodeURIComponent(sessionId)}`),
  switchSessionModel: (
    sessionId: string,
    input: {
      model: string;
      expectedModel: string | null;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAgentSession>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/model-switch`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  getSessionProviderCapabilities: (sessionId: string) =>
    controlPlaneRequest<ProviderCapabilityProjection>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/provider-capabilities`,
    ),
  listExecutionTargets: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneExecutionTarget> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets`,
    ),
  listWorkers: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneWorker> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/workers`,
    ),
  revokeWorker: (
    tenantId: string,
    workerId: string,
    input: { expectedIncarnation: number; reason: string },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneWorkerRevocationResult>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/workers/${encodeURIComponent(workerId)}/revoke`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  listWorkerManifests: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneWorkerManifest> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/worker-manifests`,
    ),
  listWorkerReleases: (tenantId: string, targetId: string) =>
    controlPlaneRequest<ControlPlaneWorkerReleaseOverview>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases`,
    ),
  getExecutionPlacement: (tenantId: string, targetId: string) =>
    controlPlaneRequest<ControlPlaneExecutionPlacementState>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-pools`,
    ),
  createWorkerPool: (tenantId: string, targetId: string, input: ControlPlaneWorkerPoolInput) =>
    controlPlaneRequest<ControlPlaneWorkerPool>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-pools`,
      { method: "POST", body: input },
    ),
  updateWorkerPool: (
    tenantId: string,
    targetId: string,
    poolId: string,
    input: ControlPlaneWorkerPoolInput & { expectedVersion: number },
  ) =>
    controlPlaneRequest<ControlPlaneWorkerPool>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-pools/${encodeURIComponent(poolId)}`,
      { method: "PATCH", body: input },
    ),
  updateExecutionPlacementPolicy: (
    tenantId: string,
    targetId: string,
    input: {
      expectedVersion: number;
      defaultPoolId: string;
      balancedPoolId: string | null;
      lowLatencyPoolId: string | null;
    },
  ) =>
    controlPlaneRequest<ControlPlaneExecutionPlacementState>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/placement-policy`,
      { method: "PUT", body: input },
    ),
  createWorkerRelease: (
    tenantId: string,
    targetId: string,
    input: {
      workerManifestId: string;
      description: string;
      gvisorCompatibleProviders?: ReadonlyArray<ProviderHostProviderKind>;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneWorkerReleaseRevision>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  transitionWorkerRelease: (
    tenantId: string,
    targetId: string,
    revisionId: string,
    action: "canary" | "promote" | "rollback",
    input: { expectedPolicyVersion: number; reason: string; canaryPercent?: number },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneWorkerReleasePolicy>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/worker-releases/${encodeURIComponent(revisionId)}/${action}`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  createExecutionTarget: (
    tenantId: string,
    input: {
      organizationId?: string;
      kind: ControlPlaneExecutionTargetKind;
      name: string;
      configuration: Record<string, unknown>;
      capabilities: Record<string, unknown>;
    },
  ) =>
    controlPlaneRequest<ControlPlaneExecutionTarget>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets`,
      { method: "POST", body: input },
    ),
  updateExecutionTargetProviderPolicy: (
    tenantId: string,
    targetId: string,
    experimentalProviders: ReadonlyArray<ProviderHostProviderKind>,
  ) =>
    controlPlaneRequest<ControlPlaneExecutionTarget>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/provider-policy`,
      { method: "PATCH", body: { experimentalProviders } },
    ),
  updateExecutionTargetRuntimeIsolationPolicy: (
    tenantId: string,
    targetId: string,
    policy: ControlPlaneRuntimeIsolationPolicyInput,
  ) =>
    controlPlaneRequest<ControlPlaneExecutionTarget>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/runtime-isolation-policy`,
      { method: "PUT", body: policy },
    ),
  provisionSSHExecutionTarget: (
    tenantId: string,
    targetId: string,
    operation: ControlPlaneSSHProvisionResult["operation"],
  ) =>
    controlPlaneRequest<ControlPlaneSSHProvisionResult>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/ssh/${operation}`,
      { method: "POST" },
    ),
  createTurn: (
    sessionId: string,
    inputText: string,
    options?: ControlPlaneIdempotencyOptions,
    modes?: { runtimeMode: RuntimeMode; interactionMode: ProviderInteractionMode },
    sourceProposedPlan?: NonNullable<OrchestrationLatestTurn["sourceProposedPlan"]>,
  ) =>
    controlPlaneRequest<ControlPlaneAgentTurn>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { inputText, ...modes, ...(sourceProposedPlan ? { sourceProposedPlan } : {}) },
      },
    ),
  compactSession: (
    sessionId: string,
    expectedLastEventSequence: number,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAdvancedCommandResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/compact`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { expectedLastEventSequence },
      },
    ),
  startReview: (
    sessionId: string,
    input: {
      expectedLastEventSequence: number;
      runtimeMode: RuntimeMode;
      target: ControlPlaneReviewTarget;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneAdvancedCommandResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/reviews`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  rollbackSession: (
    sessionId: string,
    input: { expectedLastEventSequence: number; fromTurnId: string },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneRollbackResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/rollback`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  forkSession: (
    sessionId: string,
    input: {
      expectedLastEventSequence: number;
      title: string;
      visibility: ControlPlaneAgentSession["visibility"];
      providerCredentialId?: string;
    },
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneForkResult>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/fork`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: input,
      },
    ),
  steerActiveTurn: (
    sessionId: string,
    inputText: string,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneControlCommand>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/active/steer`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { inputText },
      },
    ),
  interruptActiveTurn: (sessionId: string, options?: ControlPlaneIdempotencyOptions) =>
    controlPlaneRequest<ControlPlaneControlCommand>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/active/interrupt`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
      },
    ),
  resumeActiveTurn: (sessionId: string, options?: ControlPlaneIdempotencyOptions) =>
    controlPlaneRequest<ControlPlaneExecutionResume>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/turns/active/resume`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
      },
    ),
  listPendingInteractions: (sessionId: string) =>
    controlPlaneRequest<ControlPlanePendingInteractionSnapshot>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/interactions`,
    ),
  listExecutionRuntimeIsolationDecisions: (executionId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneRuntimeIsolationDecision> }>(
      `/v1/executions/${encodeURIComponent(executionId)}/runtime-isolation`,
    ),
  resolveApproval: (
    executionId: string,
    requestId: string,
    decision: "accept" | "decline",
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneInteractionResolution>(
      `/v1/executions/${encodeURIComponent(executionId)}/approvals/${encodeURIComponent(requestId)}/resolve`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { decision },
      },
    ),
  resolveUserInput: (
    executionId: string,
    requestId: string,
    answers: ProviderUserInputAnswers,
    options?: ControlPlaneIdempotencyOptions,
  ) =>
    controlPlaneRequest<ControlPlaneInteractionResolution>(
      `/v1/executions/${encodeURIComponent(executionId)}/user-input/${encodeURIComponent(requestId)}/resolve`,
      {
        method: "POST",
        ...idempotencyRequestHeaders(options),
        body: { answers },
      },
    ),
  getSessionUsage: (sessionId: string) =>
    controlPlaneRequest<ControlPlaneSessionUsage>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/usage`,
    ),
  listSessionEvents: (sessionId: string, afterSequence = 0, limit = 50) => {
    const query = new URLSearchParams({
      afterSequence: String(normalizeSessionEventSequence(afterSequence)),
      limit: String(Math.max(1, Math.min(200, limit))),
    });
    return controlPlaneRequest<ControlPlaneSessionEventPage>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/events?${query.toString()}`,
    );
  },
  listArtifacts: (sessionId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneArtifact> }>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts`,
    ),
  createArtifact: (
    sessionId: string,
    input: {
      kind: ControlPlaneArtifactKind;
      originalName?: string;
      executionId?: string;
      expiresAt?: string;
    },
  ) =>
    controlPlaneRequest<ControlPlaneArtifactUploadGrant>(
      `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts`,
      { method: "POST", body: input },
    ),
  uploadArtifactPayload,
  completeArtifact: (
    artifactId: string,
    input: { sizeBytes: number; sha256: string; contentType: string },
  ) =>
    controlPlaneRequest<ControlPlaneArtifact>(
      `/v1/artifacts/${encodeURIComponent(artifactId)}/complete`,
      { method: "POST", body: input },
    ),
  issueArtifactDownload: (artifactId: string) =>
    controlPlaneRequest<ControlPlaneArtifactDownloadGrant>(
      `/v1/artifacts/${encodeURIComponent(artifactId)}/download`,
      { method: "POST" },
    ),
  deleteArtifact: (artifactId: string) =>
    controlPlaneRequest<void>(`/v1/artifacts/${encodeURIComponent(artifactId)}`, {
      method: "DELETE",
    }),
  subscribeSessionEvents,
  listTenantMembers: (tenantId: string) =>
    controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneTenantMember> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/members`,
    ),
  listTenantOutboxMessages: (
    tenantId: string,
    input: { status: "all" | "pending" | "retrying" | "dead-letter" | "published"; limit?: number },
  ) => {
    const query = new URLSearchParams({
      status: input.status,
      limit: String(Math.max(1, Math.min(200, input.limit ?? 100))),
    });
    return controlPlaneRequest<{ items: ReadonlyArray<ControlPlaneOutboxMessage> }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/outbox-messages?${query.toString()}`,
    );
  },
  replayTenantOutboxMessage: (tenantId: string, messageId: string) =>
    controlPlaneRequest<{ id: string; topic: string; messageKey: string; attempts: number }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/outbox-messages/${encodeURIComponent(messageId)}/replay`,
      { method: "POST" },
    ),
  inviteTenantMember: (tenantId: string, input: { email: string; role: string }) =>
    controlPlaneRequest<TenantInvitation>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/invitations`,
      { method: "POST", body: input },
    ),
  updateTenantMember: (
    tenantId: string,
    userId: string,
    input: { role?: string; status?: "active" | "suspended" },
  ) =>
    controlPlaneRequest<ControlPlaneTenantMember>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/members/${encodeURIComponent(userId)}`,
      { method: "PATCH", body: input },
    ),
  revokeTenantUserSessions: (tenantId: string, userId: string) =>
    controlPlaneRequest<{ revokedCount: number }>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/members/${encodeURIComponent(userId)}/revoke-sessions`,
      { method: "POST" },
    ),
  removeTenantMember: (tenantId: string, userId: string) =>
    controlPlaneRequest<void>(
      `/v1/tenants/${encodeURIComponent(tenantId)}/members/${encodeURIComponent(userId)}`,
      { method: "DELETE" },
    ),
};
