# Generated from docs/api/openapi.yaml. Do not edit by hand.
from __future__ import annotations

from typing import Any, Final, Literal, Required, TypeAlias, TypedDict

class Artifact(TypedDict, total=False):
    id: Required[UUID]
    tenantId: Required[UUID]
    organizationId: Required[UUID]
    projectId: Required[UUID]
    sessionId: Required[UUID]
    executionId: Required[UUID | None]
    kind: Required[Literal["attachment", "generated_file", "terminal_log", "diff", "workspace_snapshot", "checkpoint", "memory"]]
    status: Required[Literal["pending", "ready", "deleting", "deleted", "failed"]]
    originalName: Required[str | None]
    contentType: Required[str | None]
    sizeBytes: Required[int | None]
    sha256: Required[str | None]
    createdByType: Required[Literal["user", "service_account", "worker"]]
    createdById: Required[UUID]
    readyAt: Required[str | None]
    createdAt: Required[str]
    expiresAt: Required[str | None]
    deletedAt: Required[str | None]

class ArtifactPage(TypedDict, total=False):
    items: Required[list[Artifact]]
    nextCursor: Required[str | None]

class ArtifactDownloadGrant(TypedDict, total=False):
    artifact: Required[Artifact]
    url: Required[str]
    expiresAt: Required[str]

class ArtifactUploadGrant(TypedDict, total=False):
    artifact: Required[Artifact]
    uploadRequired: Required[bool]
    method: Literal["PUT"]
    url: str
    headers: dict[str, Any]
    expiresAt: str

class CreateArtifactRequest(TypedDict, total=False):
    kind: Required[Literal["attachment", "generated_file", "terminal_log", "diff", "workspace_snapshot"]]
    originalName: str | None
    executionId: UUID | None
    expiresAt: str | None

class CompleteArtifactRequest(TypedDict, total=False):
    sizeBytes: Required[int]
    sha256: Required[str]
    contentType: Required[str]

class CreateSessionRequest(TypedDict, total=False):
    title: Required[str]
    visibility: Literal["project", "organization"]
    provider: Required[str]
    model: str | None
    providerCredentialId: UUID | None
    executionTargetId: UUID | None
    executionTargetGroupId: UUID | None
    preferredExecutionRegion: str | None
    resourceLifecyclePolicy: ResourceLifecycleOverrides | None

class Session(TypedDict, total=False):
    id: Required[UUID]
    tenantId: Required[UUID]
    organizationId: Required[UUID]
    projectId: Required[UUID]
    createdBy: Required[UUID]
    title: Required[str]
    status: Required[str]
    visibility: Required[Literal["project", "organization"]]
    provider: Required[str]
    model: Required[str | None]
    providerCredentialId: Required[UUID | None]
    executionTargetId: Required[UUID]
    requestedExecutionTargetId: Required[UUID]
    executionTargetGroupId: UUID | None
    routingPolicyVersion: int | None
    preferredExecutionRegion: str | None
    forkSourceSessionId: UUID | None
    forkSourceTurnId: UUID | None
    forkSourceEventSequence: int | None
    forkStrategy: str | None
    lastEventSequence: Required[int]
    resourceState: Required[str]
    meaningfulActivityAt: Required[str]
    resourceIdleSince: str | None
    absoluteExpiresAt: str | None
    resourceLifecyclePolicy: Required[EffectiveResourceLifecyclePolicy]
    createdAt: Required[str]
    updatedAt: Required[str]
    archivedAt: Required[str | None]

class SessionPage(TypedDict, total=False):
    items: Required[list[Session]]
    nextCursor: Required[str | None]

class CreateTurnRequest(TypedDict, total=False):
    inputText: Required[str]
    runtimeMode: Required[str]
    interactionMode: Required[str]
    sourceProposedPlan: SourceProposedPlanReference | None
    automationId: UUID | None
    queueClass: str
    queuePriority: int
    quotaUnits: int

class Turn(TypedDict, total=False):
    id: Required[UUID]
    tenantId: Required[UUID]
    sessionId: Required[UUID]
    createdBy: Required[UUID]
    status: Required[str]
    inputText: Required[str]
    turnKind: Required[str]
    runtimeMode: Required[str]
    interactionMode: Required[str]
    startedAt: Required[str | None]
    completedAt: Required[str | None]
    createdAt: Required[str]

class SessionEvent(TypedDict, total=False):
    eventId: Required[UUID]
    eventVersion: Required[int]
    tenantId: Required[UUID]
    organizationId: Required[UUID]
    projectId: Required[UUID]
    sessionId: Required[UUID]
    executionId: Required[UUID | None]
    workerId: Required[UUID | None]
    generation: Required[int | None]
    sequence: Required[int]
    eventType: Required[str]
    actorType: Required[Literal["user", "service_account", "worker", "system"]]
    actorId: Required[UUID | None]
    payload: Required[dict[str, Any]]
    occurredAt: Required[str]

class SessionEventPage(TypedDict, total=False):
    items: Required[list[SessionEvent]]
    lastSequence: Required[int]

class ResolveApprovalRequest(TypedDict, total=False):
    decision: Required[Literal["accept", "decline"]]

class ResolveUserInputRequest(TypedDict, total=False):
    answers: Required[dict[str, Any]]

class Interaction(TypedDict, total=False):
    id: Required[UUID]
    executionId: Required[UUID]
    sessionId: Required[UUID]
    turnId: Required[UUID]
    provider: Required[str]
    requestId: Required[str]
    kind: Required[Literal["approval", "user-input"]]
    status: Required[str]
    payload: Required[dict[str, Any]]
    resolution: dict[str, Any]
    requestedAt: Required[str]
    expiresAt: Required[str]
    resolvedAt: Required[str | None]

class InteractionPage(TypedDict, total=False):
    items: Required[list[Interaction]]
    nextCursor: Required[str | None]

class PendingInteraction(TypedDict, total=False):
    id: Required[UUID]
    executionId: Required[UUID]
    turnId: Required[UUID]
    provider: Required[str]
    requestId: Required[str]
    kind: Required[Literal["approval", "user-input"]]
    payload: Required[dict[str, Any]]
    requestedAt: Required[str]
    expiresAt: Required[str]

class PendingInteractionPage(TypedDict, total=False):
    items: Required[list[PendingInteraction]]
    nextCursor: Required[str | None]
    snapshotSequence: Required[int]

class CreateProjectRequest(TypedDict, total=False):
    name: Required[str]
    repositoryUrl: str | None
    defaultBranch: str
    visibility: Literal["organization", "tenant"]

class UpdateProjectRequest(TypedDict, total=False):
    name: str
    repositoryUrl: str
    defaultBranch: str
    visibility: Literal["organization", "tenant"]

class ProviderCapabilityProjection(TypedDict, total=False):
    executionTargetId: Required[UUID]
    targetKind: Required[str]
    basis: Required[Literal["target", "execution"]]
    executionId: str | None
    items: Required[list[ProviderCapabilityItem]]

class ProviderCapabilityItem(TypedDict, total=False):
    provider: Required[str]
    capabilityId: Required[str]
    status: Required[Literal["supported", "unsupported", "unobserved"]]
    reasonCode: Required[str]
    supportMode: Literal["native", "emulated"]

class SwitchSessionModelRequest(TypedDict, total=False):
    model: Required[str]
    expectedModel: Required[str | None]

class SessionUsage(TypedDict, total=False):
    tenantId: Required[UUID]
    sessionId: Required[UUID]
    items: Required[list[ExecutionUsage]]

class ExecutionUsage(TypedDict, total=False):
    executionId: Required[UUID]
    generation: Required[int]
    turnId: Required[UUID]
    provider: Required[str]
    model: Required[str | None]
    inputTokens: Required[int]
    cachedInputTokens: Required[int]
    outputTokens: Required[int]
    reasoningTokens: Required[int]
    totalTokens: Required[int]
    networkIngressBytes: Required[int]
    networkEgressBytes: Required[int]
    durationMillis: Required[int]
    providerCostMicros: Required[int]
    providerCostReported: Required[bool]
    providerCurrency: Required[str]
    platformCharges: Required[list[PlatformCharge]]
    totalCostByCurrency: Required[dict[str, Any]]
    costCoverage: Required[Literal["provider-unavailable", "provider-only", "provider-and-allocated-platform", "provider-unavailable-with-allocated-platform"]]
    final: Required[bool]
    updatedAt: Required[str]

class PlatformCharge(TypedDict, total=False):
    kind: Required[str]
    currencyCode: Required[str]
    amountMicros: Required[int]
    source: Required[Literal["estimated", "actual"]]

class DeveloperExecution(TypedDict, total=False):
    id: Required[UUID]
    sessionId: Required[UUID]
    turnId: Required[UUID]
    attempt: Required[int]
    status: Required[str]
    executionTargetId: Required[UUID]
    targetKind: Required[str]
    provider: Required[str | None]
    queuedAt: Required[str]
    startedAt: Required[str | None]
    finishedAt: Required[str | None]
    failureCode: Required[str | None]

class DeveloperControlCommand(TypedDict, total=False):
    id: Required[UUID]
    executionId: Required[UUID]
    sessionId: Required[UUID]
    turnId: Required[UUID]
    provider: Required[str]
    commandType: Required[str]
    status: Required[str]
    requestedAt: Required[str]

class SteerActiveTurnRequest(TypedDict, total=False):
    inputText: Required[str]

class CompactSessionRequest(TypedDict, total=False):
    expectedLastEventSequence: Required[int]

class ReviewTarget(TypedDict, total=False):
    type: Required[Literal["uncommittedChanges", "baseBranch"]]
    branch: str

class StartSessionReviewRequest(TypedDict, total=False):
    expectedLastEventSequence: Required[int]
    target: Required[ReviewTarget]
    runtimeMode: Literal["approval-required", "full-access"]

class DeveloperQueuedSessionOperation(TypedDict, total=False):
    type: Required[Literal["compact", "review"]]
    turn: Required[Turn]
    executionId: Required[UUID]
    controlCommand: Required[DeveloperControlCommand]

class RollbackSessionRequest(TypedDict, total=False):
    expectedLastEventSequence: Required[int]
    fromTurnId: Required[UUID]

class RollbackSessionResult(TypedDict, total=False):
    sessionId: Required[UUID]
    eventId: Required[UUID]
    eventSequence: Required[int]
    fromSessionId: Required[UUID]
    fromTurnId: Required[UUID]
    fromSequence: Required[int]
    removedTurnCount: Required[int]
    supportMode: Required[Literal["emulated"]]
    workspaceDisposition: Required[str]
    externalSideEffectsReverted: Required[bool]

class ForkSessionRequest(TypedDict, total=False):
    expectedLastEventSequence: Required[int]
    title: str
    visibility: Literal["project", "organization"]
    providerCredentialId: UUID
    executionTargetId: UUID

class ForkSessionResult(TypedDict, total=False):
    session: Required[Session]
    sourceSessionId: Required[UUID]
    sourceEventSequence: Required[int]
    supportMode: Required[Literal["emulated"]]

class Project(TypedDict, total=False):
    id: Required[UUID]
    tenantId: Required[UUID]
    organizationId: Required[UUID]
    name: Required[str]
    repositoryUrl: Required[str | None]
    defaultBranch: Required[str]
    gitCredentialId: Required[UUID | None]
    visibility: Required[Literal["organization", "tenant"]]
    createdBy: Required[UUID]
    createdAt: Required[str]
    updatedAt: Required[str]
    archivedAt: Required[str | None]

class ProjectPage(TypedDict, total=False):
    items: Required[list[Project]]
    nextCursor: Required[str | None]

class CreateExecutionTargetRequest(TypedDict, total=False):
    organizationId: Required[UUID]
    kind: Required[Literal["ssh", "docker", "kubernetes"]]
    name: Required[str]
    configuration: Required[dict[str, Any]]
    capabilities: Required[dict[str, Any]]

class ExecutionTarget(TypedDict, total=False):
    id: Required[UUID]
    tenantId: Required[UUID | None]
    organizationId: Required[UUID | None]
    kind: Required[Literal["local", "ssh", "docker", "kubernetes"]]
    name: Required[str]
    status: Required[Literal["active", "disabled", "offline"]]
    capabilities: Required[dict[str, Any]]
    isolationProfile: Required[str]
    platformSharedEligible: Required[bool]
    productBoundary: Required[Literal["single-tenant-trusted", "multi-tenant-restricted"]]
    runtimeIsolationPolicy: dict[str, Any]
    runtimeIsolationStatus: dict[str, Any]
    createdAt: Required[str]
    updatedAt: Required[str]

class CreateExecutionTargetProvisioningOperationRequest(TypedDict, total=False):
    action: Required[Literal["install", "upgrade", "revoke"]]

class ExecutionTargetProvisioningOperation(TypedDict, total=False):
    id: Required[UUID]
    targetId: Required[UUID]
    action: Required[Literal["install", "upgrade", "revoke"]]
    state: Required[Literal["accepted", "running", "succeeded", "failed"]]
    attempt: Required[int]
    result: dict[str, Any]
    error: dict[str, Any]
    createdAt: Required[str]
    updatedAt: Required[str]
    startedAt: str
    completedAt: str

class ErrorEnvelope(TypedDict, total=False):
    error: Required[dict[str, Any]]

UUID: TypeAlias = str

class ResourceLifecycleOverrides(TypedDict, total=False):
    waitingKeepAliveSeconds: int | None
    suspendAfterIdleSeconds: int | None
    absoluteSessionLifetimeSeconds: int | None
    workspaceRetentionDays: int | None
    warmPoolMode: str | None

class EffectiveResourceLifecyclePolicy(TypedDict, total=False):
    waitingKeepAliveSeconds: Required[int]
    suspendAfterIdleSeconds: Required[int]
    absoluteSessionLifetimeSeconds: Required[int | None]
    workspaceRetentionDays: Required[int]
    warmPoolMode: Required[str]

class SourceProposedPlanReference(TypedDict, total=False):
    threadId: Required[str]
    planId: Required[str]

OPERATIONS: Final[dict[str, tuple[str, str]]] = {
    "archiveProject": ("DELETE", "/v1/projects/{projectID}"),
    "archiveSession": ("POST", "/v1/sessions/{sessionID}/archive"),
    "cancelExecution": ("POST", "/v1/executions/{executionID}/cancel"),
    "compactSession": ("POST", "/v1/sessions/{sessionID}/compact"),
    "completeArtifact": ("POST", "/v1/artifacts/{artifactID}/complete"),
    "createArtifact": ("POST", "/v1/sessions/{sessionID}/artifacts"),
    "createExecutionTarget": ("POST", "/v1/tenants/{tenantID}/execution-targets"),
    "createExecutionTargetProvisioningOperation": ("POST", "/v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations"),
    "createProject": ("POST", "/v1/tenants/{tenantID}/organizations/{organizationID}/projects"),
    "createSession": ("POST", "/v1/projects/{projectID}/sessions"),
    "createTurn": ("POST", "/v1/sessions/{sessionID}/turns"),
    "deleteArtifact": ("DELETE", "/v1/artifacts/{artifactID}"),
    "downloadArtifact": ("POST", "/v1/artifacts/{artifactID}/download"),
    "forkSession": ("POST", "/v1/sessions/{sessionID}/fork"),
    "getAgentSession": ("GET", "/v1/sessions/{sessionID}"),
    "getArtifact": ("GET", "/v1/artifacts/{artifactID}"),
    "getExecutionTarget": ("GET", "/v1/tenants/{tenantID}/execution-targets/{executionTargetID}"),
    "getExecutionTargetProvisioningOperation": ("GET", "/v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations/{provisioningOperationID}"),
    "getProject": ("GET", "/v1/projects/{projectID}"),
    "getSessionUsage": ("GET", "/v1/sessions/{sessionID}/usage"),
    "interruptActiveTurn": ("POST", "/v1/sessions/{sessionID}/turns/active/interrupt"),
    "listArtifacts": ("GET", "/v1/sessions/{sessionID}/artifacts"),
    "listExecutionInteractions": ("GET", "/v1/executions/{executionID}/interactions"),
    "listPendingSessionInteractions": ("GET", "/v1/sessions/{sessionID}/interactions"),
    "listProjects": ("GET", "/v1/tenants/{tenantID}/organizations/{organizationID}/projects"),
    "listProjectSessions": ("GET", "/v1/projects/{projectID}/sessions"),
    "listSessionEvents": ("GET", "/v1/sessions/{sessionID}/events"),
    "projectProviderCapabilities": ("GET", "/v1/projects/{projectID}/provider-capabilities"),
    "resolveExecutionApproval": ("POST", "/v1/executions/{executionID}/approvals/{requestID}/resolve"),
    "resolveExecutionUserInput": ("POST", "/v1/executions/{executionID}/user-input/{requestID}/resolve"),
    "resumeActiveTurn": ("POST", "/v1/sessions/{sessionID}/turns/active/resume"),
    "resumeActiveTurnExecution": ("POST", "/v1/executions/{executionID}/resume"),
    "resumeSession": ("POST", "/v1/sessions/{sessionID}/resume"),
    "rollbackSession": ("POST", "/v1/sessions/{sessionID}/rollback"),
    "sessionProviderCapabilities": ("GET", "/v1/sessions/{sessionID}/provider-capabilities"),
    "startSessionReview": ("POST", "/v1/sessions/{sessionID}/reviews"),
    "steerActiveTurn": ("POST", "/v1/sessions/{sessionID}/turns/active/steer"),
    "streamSessionEvents": ("GET", "/v1/sessions/{sessionID}/events/stream"),
    "suspendSession": ("POST", "/v1/sessions/{sessionID}/suspend"),
    "switchSessionModel": ("POST", "/v1/sessions/{sessionID}/model-switch"),
    "updateProject": ("PATCH", "/v1/projects/{projectID}"),
}
