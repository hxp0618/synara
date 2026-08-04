import type { components } from "./generated/openapi";

export type Artifact = components["schemas"]["Artifact"];
export type ArtifactPage = components["schemas"]["ArtifactPage"];
export type ArtifactDownloadGrant = components["schemas"]["ArtifactDownloadGrant"];
export type ArtifactUploadGrant = components["schemas"]["ArtifactUploadGrant"];
export type CreateArtifactInput = components["schemas"]["CreateArtifactRequest"];
export type CompleteArtifactInput = components["schemas"]["CompleteArtifactRequest"];

export type CreateSessionInput = components["schemas"]["CreateSessionRequest"];
export type Session = components["schemas"]["Session"];
export type SessionPage = components["schemas"]["SessionPage"];
export type CreateTurnInput = components["schemas"]["CreateTurnRequest"];
export type Turn = components["schemas"]["Turn"];
export type SessionEvent = components["schemas"]["SessionEvent"];
export type SessionEventPage = components["schemas"]["SessionEventPage"];
export type ApprovalDecision = components["schemas"]["ResolveApprovalRequest"]["decision"];
export type Interaction = components["schemas"]["Interaction"];
export type InteractionPage = components["schemas"]["InteractionPage"];
export type PendingInteraction = components["schemas"]["PendingInteraction"];
export type PendingInteractionPage = components["schemas"]["PendingInteractionPage"];
export type UserInputAnswers = components["schemas"]["ResolveUserInputRequest"]["answers"];
export type CreateProjectInput = components["schemas"]["CreateProjectRequest"];
export type UpdateProjectInput = components["schemas"]["UpdateProjectRequest"];
export type ProviderCapabilityProjection = components["schemas"]["ProviderCapabilityProjection"];
export type ProviderCapabilityItem = components["schemas"]["ProviderCapabilityItem"];
export type SwitchSessionModelInput = components["schemas"]["SwitchSessionModelRequest"];
export type SessionUsage = components["schemas"]["SessionUsage"];
export type ExecutionUsage = components["schemas"]["ExecutionUsage"];
export type PlatformCharge = components["schemas"]["PlatformCharge"];
export type DeveloperExecution = components["schemas"]["DeveloperExecution"];
export type DeveloperControlCommand = components["schemas"]["DeveloperControlCommand"];
export type SteerActiveTurnInput = components["schemas"]["SteerActiveTurnRequest"];
export type CompactSessionInput = components["schemas"]["CompactSessionRequest"];
export type ReviewTarget = components["schemas"]["ReviewTarget"];
export type StartSessionReviewInput = components["schemas"]["StartSessionReviewRequest"];
export type DeveloperQueuedSessionOperation =
  components["schemas"]["DeveloperQueuedSessionOperation"];
export type RollbackSessionInput = components["schemas"]["RollbackSessionRequest"];
export type RollbackSessionResult = components["schemas"]["RollbackSessionResult"];
export type ForkSessionInput = components["schemas"]["ForkSessionRequest"];
export type ForkSessionResult = components["schemas"]["ForkSessionResult"];
export type Project = components["schemas"]["Project"];
export type ProjectPage = components["schemas"]["ProjectPage"];
export type CreateExecutionTargetInput = components["schemas"]["CreateExecutionTargetRequest"];
export type ExecutionTarget = components["schemas"]["ExecutionTarget"];
export type ProvisioningAction =
  components["schemas"]["CreateExecutionTargetProvisioningOperationRequest"]["action"];
export type ExecutionTargetProvisioningOperation =
  components["schemas"]["ExecutionTargetProvisioningOperation"];

export type RateLimit = {
  limit: number | null;
  remaining: number | null;
  resetAfterSeconds: number | null;
};

export type ResponseMetadata = {
  requestId: string | null;
  idempotencyReplayed: boolean;
  rateLimit: RateLimit;
};

export type SDKResponse<T> = ResponseMetadata & { data: T };
