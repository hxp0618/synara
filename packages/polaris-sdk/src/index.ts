import { PolarisError, PolarisSequenceGapError, PolarisTransportError } from "./errors";
import { streamSessionEvents, type SessionEventStreamOptions } from "./sse";
import { PolarisTransport, type TransportOptions } from "./transport";
import type {
  Artifact,
  ArtifactDownloadGrant,
  ArtifactPage,
  ArtifactUploadGrant,
  CompleteArtifactInput,
  CreateArtifactInput,
  ApprovalDecision,
  CreateProjectInput,
  UpdateProjectInput,
  CreateExecutionTargetInput,
  CreateSessionInput,
  CreateTurnInput,
  Interaction,
  InteractionPage,
  PendingInteraction,
  PendingInteractionPage,
  ProviderCapabilityProjection,
  UserInputAnswers,
  Project,
  ProjectPage,
  ExecutionTarget,
  ExecutionTargetProvisioningOperation,
  ProvisioningAction,
  ResponseMetadata,
  SDKResponse,
  Session,
  SessionPage,
  SessionEvent,
  SessionEventPage,
  SwitchSessionModelInput,
  SessionUsage,
  DeveloperExecution,
  DeveloperControlCommand,
  SteerActiveTurnInput,
  CompactSessionInput,
  StartSessionReviewInput,
  DeveloperQueuedSessionOperation,
  RollbackSessionInput,
  RollbackSessionResult,
  ForkSessionInput,
  ForkSessionResult,
  Turn,
} from "./types";

export { PolarisError, PolarisSequenceGapError, PolarisTransportError };
export type {
  Artifact,
  ArtifactDownloadGrant,
  ArtifactPage,
  ArtifactUploadGrant,
  CompleteArtifactInput,
  CreateArtifactInput,
  ApprovalDecision,
  CreateProjectInput,
  UpdateProjectInput,
  CreateExecutionTargetInput,
  CreateSessionInput,
  CreateTurnInput,
  Interaction,
  InteractionPage,
  PendingInteraction,
  PendingInteractionPage,
  ProviderCapabilityItem,
  ProviderCapabilityProjection,
  UserInputAnswers,
  Project,
  ProjectPage,
  ExecutionTarget,
  ExecutionTargetProvisioningOperation,
  ProvisioningAction,
  RateLimit,
  ResponseMetadata,
  SDKResponse,
  Session,
  SessionPage,
  SessionEvent,
  SessionEventPage,
  SwitchSessionModelInput,
  SessionUsage,
  ExecutionUsage,
  PlatformCharge,
  DeveloperExecution,
  DeveloperControlCommand,
  SteerActiveTurnInput,
  CompactSessionInput,
  ReviewTarget,
  StartSessionReviewInput,
  DeveloperQueuedSessionOperation,
  RollbackSessionInput,
  RollbackSessionResult,
  ForkSessionInput,
  ForkSessionResult,
  Turn,
} from "./types";
export type { SessionEventStreamOptions } from "./sse";

export type PolarisOptions = TransportOptions;
export type IdempotencyOptions = { idempotencyKey?: string; signal?: AbortSignal };
export type CreateSessionParameters = CreateSessionInput & { projectId: string };
export type WaitForProvisioningOptions = {
  pollIntervalMs?: number;
  timeoutMs?: number;
  signal?: AbortSignal;
};

export class Polaris {
  readonly artifacts: ArtifactsResource;
  readonly projects: ProjectsResource;
  readonly sessions: SessionsResource;
  readonly interactions: InteractionsResource;
  readonly executions: ExecutionsResource;
  readonly approvals: ApprovalsResource;
  readonly targets: TargetsResource;

  constructor(options: PolarisOptions) {
    const transport = new PolarisTransport(options);
    this.artifacts = new ArtifactsResource(transport);
    this.projects = new ProjectsResource(transport);
    this.sessions = new SessionsResource(transport);
    this.interactions = new InteractionsResource(transport);
    this.executions = new ExecutionsResource(transport);
    this.approvals = new ApprovalsResource(transport);
    this.targets = new TargetsResource(transport);
  }
}

export class ExecutionsResource {
  constructor(private readonly transport: PolarisTransport) {}

  cancel(
    executionId: string,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<DeveloperExecution>> {
    return this.transport.json<DeveloperExecution>({
      method: "POST",
      path: `/v1/executions/${encodeURIComponent(executionId)}/cancel`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  resumeActiveTurn(
    executionId: string,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<DeveloperExecution>> {
    return this.transport.json<DeveloperExecution>({
      method: "POST",
      path: `/v1/executions/${encodeURIComponent(executionId)}/resume`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }
}

export class ArtifactsResource {
  constructor(private readonly transport: PolarisTransport) {}

  create(
    sessionId: string,
    input: CreateArtifactInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<ArtifactUploadGrant>> {
    return this.transport.json<ArtifactUploadGrant>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  list(
    sessionId: string,
    options: { limit?: number; cursor?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ArtifactPage>> {
    const query = boundedPageQuery(options, "Artifact");
    return this.transport.json<ArtifactPage>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(sessionId)}/artifacts?${query}`,
      signal: options.signal,
    });
  }

  get(artifactId: string, options: { signal?: AbortSignal } = {}): Promise<SDKResponse<Artifact>> {
    return this.transport.json<Artifact>({
      method: "GET",
      path: `/v1/artifacts/${encodeURIComponent(artifactId)}`,
      signal: options.signal,
    });
  }

  download(
    artifactId: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ArtifactDownloadGrant>> {
    return this.transport.json<ArtifactDownloadGrant>({
      method: "POST",
      path: `/v1/artifacts/${encodeURIComponent(artifactId)}/download`,
      signal: options.signal,
    });
  }

  complete(
    artifactId: string,
    input: CompleteArtifactInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Artifact>> {
    return this.transport.json<Artifact>({
      method: "POST",
      path: `/v1/artifacts/${encodeURIComponent(artifactId)}/complete`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  async delete(artifactId: string, options: IdempotencyOptions = {}): Promise<ResponseMetadata> {
    const response = await this.transport.json<null>({
      method: "DELETE",
      path: `/v1/artifacts/${encodeURIComponent(artifactId)}`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
    const { data: _data, ...metadata } = response;
    return metadata;
  }
}

export class InteractionsResource {
  constructor(private readonly transport: PolarisTransport) {}

  list(
    executionId: string,
    options: { limit?: number; cursor?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<InteractionPage>> {
    const query = boundedPageQuery(options, "Interaction");
    return this.transport.json<InteractionPage>({
      method: "GET",
      path: `/v1/executions/${encodeURIComponent(executionId)}/interactions?${query}`,
      signal: options.signal,
    });
  }

  resolveUserInput(
    executionId: string,
    requestId: string,
    answers: UserInputAnswers,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Interaction>> {
    return this.transport.json<Interaction>({
      method: "POST",
      path: `/v1/executions/${encodeURIComponent(executionId)}/user-input/${encodeURIComponent(requestId)}/resolve`,
      body: { answers },
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }
}

export class ProjectsResource {
  constructor(private readonly transport: PolarisTransport) {}

  list(
    tenantId: string,
    organizationId: string,
    options: { limit?: number; cursor?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ProjectPage>> {
    const limit = normalizePageLimit(options.limit, "Project");
    const query = new URLSearchParams({ limit: String(limit) });
    if (options.cursor !== undefined) {
      if (options.cursor.trim() === "") throw new TypeError("cursor must not be empty.");
      query.set("cursor", options.cursor);
    }
    return this.transport.json<ProjectPage>({
      method: "GET",
      path: `${projectsPath(tenantId, organizationId)}?${query.toString()}`,
      signal: options.signal,
    });
  }

  create(
    tenantId: string,
    organizationId: string,
    input: CreateProjectInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Project>> {
    return this.transport.json<Project>({
      method: "POST",
      path: projectsPath(tenantId, organizationId),
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  get(projectId: string, options: { signal?: AbortSignal } = {}): Promise<SDKResponse<Project>> {
    return this.transport.json<Project>({
      method: "GET",
      path: `/v1/projects/${encodeURIComponent(projectId)}`,
      signal: options.signal,
    });
  }

  update(
    projectId: string,
    input: UpdateProjectInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Project>> {
    return this.transport.json<Project>({
      method: "PATCH",
      path: `/v1/projects/${encodeURIComponent(projectId)}`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  async archive(projectId: string, options: IdempotencyOptions = {}): Promise<ResponseMetadata> {
    const response = await this.transport.json<null>({
      method: "DELETE",
      path: `/v1/projects/${encodeURIComponent(projectId)}`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
    const { data: _data, ...metadata } = response;
    return metadata;
  }

  capabilities(
    projectId: string,
    options: { executionTargetId?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ProviderCapabilityProjection>> {
    const query = new URLSearchParams();
    if (options.executionTargetId !== undefined)
      query.set("executionTargetId", options.executionTargetId);
    const suffix = query.size === 0 ? "" : `?${query.toString()}`;
    return this.transport.json<ProviderCapabilityProjection>({
      method: "GET",
      path: `/v1/projects/${encodeURIComponent(projectId)}/provider-capabilities${suffix}`,
      signal: options.signal,
    });
  }
}

export class TargetsResource {
  constructor(private readonly transport: PolarisTransport) {}

  create(
    tenantId: string,
    input: CreateExecutionTargetInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<ExecutionTarget>> {
    return this.transport.json<ExecutionTarget>({
      method: "POST",
      path: `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  get(
    tenantId: string,
    executionTargetId: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ExecutionTarget>> {
    return this.transport.json<ExecutionTarget>({
      method: "GET",
      path: `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(executionTargetId)}`,
      signal: options.signal,
    });
  }

  provision(
    tenantId: string,
    executionTargetId: string,
    action: ProvisioningAction,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<ExecutionTargetProvisioningOperation>> {
    return this.transport.json<ExecutionTargetProvisioningOperation>({
      method: "POST",
      path: provisioningPath(tenantId, executionTargetId),
      body: { action },
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  getProvisioningOperation(
    tenantId: string,
    executionTargetId: string,
    operationId: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<SDKResponse<ExecutionTargetProvisioningOperation>> {
    return this.transport.json<ExecutionTargetProvisioningOperation>({
      method: "GET",
      path: `${provisioningPath(tenantId, executionTargetId)}/${encodeURIComponent(operationId)}`,
      signal: options.signal,
    });
  }

  async waitForProvisioning(
    tenantId: string,
    executionTargetId: string,
    operationId: string,
    options: WaitForProvisioningOptions = {},
  ): Promise<SDKResponse<ExecutionTargetProvisioningOperation>> {
    const pollIntervalMs = options.pollIntervalMs ?? 1_000;
    const timeoutMs = options.timeoutMs ?? 600_000;
    if (!Number.isSafeInteger(pollIntervalMs) || pollIntervalMs < 10 || pollIntervalMs > 60_000)
      throw new TypeError("pollIntervalMs must be an integer between 10 and 60000.");
    if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 3_600_000)
      throw new TypeError("timeoutMs must be an integer between 1 and 3600000.");
    const deadline = Date.now() + timeoutMs;
    while (true) {
      const response = await this.getProvisioningOperation(
        tenantId,
        executionTargetId,
        operationId,
        options.signal ? { signal: options.signal } : {},
      );
      if (response.data.state === "succeeded" || response.data.state === "failed") return response;
      if (Date.now() >= deadline)
        throw new PolarisTransportError(
          `Provisioning operation ${operationId} did not finish within ${timeoutMs}ms.`,
        );
      await abortableDelay(
        Math.min(pollIntervalMs, Math.max(0, deadline - Date.now())),
        options.signal,
      );
    }
  }
}

export class SessionsResource {
  constructor(private readonly transport: PolarisTransport) {}

  list(
    projectId: string,
    options: { limit?: number; cursor?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<SessionPage>> {
    const limit = normalizePageLimit(options.limit, "Session");
    const query = new URLSearchParams({ limit: String(limit) });
    if (options.cursor !== undefined) {
      if (options.cursor.trim() === "") throw new TypeError("cursor must not be empty.");
      query.set("cursor", options.cursor);
    }
    return this.transport.json<SessionPage>({
      method: "GET",
      path: `/v1/projects/${encodeURIComponent(projectId)}/sessions?${query.toString()}`,
      signal: options.signal,
    });
  }

  async get(sessionId: string, options: { signal?: AbortSignal } = {}): Promise<SessionHandle> {
    const response = await this.transport.json<Session>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(sessionId)}`,
      signal: options.signal,
    });
    const { data, ...metadata } = response;
    return new SessionHandle(this.transport, data, metadata);
  }

  async create(
    parameters: CreateSessionParameters,
    options: IdempotencyOptions = {},
  ): Promise<SessionHandle> {
    const { projectId, ...input } = parameters;
    const response = await this.transport.json<Session>({
      method: "POST",
      path: `/v1/projects/${encodeURIComponent(projectId)}/sessions`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
    const { data, ...metadata } = response;
    return new SessionHandle(this.transport, data, metadata);
  }
}

export class SessionHandle {
  readonly id: string;

  constructor(
    private readonly transport: PolarisTransport,
    readonly data: Session,
    readonly response: ResponseMetadata,
  ) {
    this.id = data.id;
  }

  sendTurn(input: CreateTurnInput, options: IdempotencyOptions = {}): Promise<SDKResponse<Turn>> {
    return this.transport.json<Turn>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/turns`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  archive(options: IdempotencyOptions = {}): Promise<SDKResponse<Session>> {
    return this.transport.json<Session>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/archive`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  capabilities(options: { signal?: AbortSignal } = {}): Promise<SDKResponse<ProviderCapabilityProjection>> {
    return this.transport.json<ProviderCapabilityProjection>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/provider-capabilities`,
      signal: options.signal,
    });
  }

  switchModel(
    input: SwitchSessionModelInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Session>> {
    return this.transport.json<Session>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/model-switch`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  suspend(options: IdempotencyOptions = {}): Promise<SDKResponse<Session>> {
    return this.transport.json<Session>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/suspend`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  resume(options: IdempotencyOptions = {}): Promise<SDKResponse<Session>> {
    return this.transport.json<Session>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/resume`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  usage(options: { signal?: AbortSignal } = {}): Promise<SDKResponse<SessionUsage>> {
    return this.transport.json<SessionUsage>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/usage`,
      signal: options.signal,
    });
  }

  interrupt(options: IdempotencyOptions = {}): Promise<SDKResponse<DeveloperControlCommand>> {
    return this.transport.json<DeveloperControlCommand>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/turns/active/interrupt`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  resumeActiveTurn(options: IdempotencyOptions = {}): Promise<SDKResponse<DeveloperExecution>> {
    return this.transport.json<DeveloperExecution>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/turns/active/resume`,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  steer(
    input: SteerActiveTurnInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<DeveloperControlCommand>> {
    return this.transport.json<DeveloperControlCommand>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/turns/active/steer`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  compact(
    input: CompactSessionInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<DeveloperQueuedSessionOperation>> {
    return this.transport.json<DeveloperQueuedSessionOperation>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/compact`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  startReview(
    input: StartSessionReviewInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<DeveloperQueuedSessionOperation>> {
    return this.transport.json<DeveloperQueuedSessionOperation>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/reviews`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  rollback(
    input: RollbackSessionInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<RollbackSessionResult>> {
    return this.transport.json<RollbackSessionResult>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/rollback`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  fork(
    input: ForkSessionInput,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<ForkSessionResult>> {
    return this.transport.json<ForkSessionResult>({
      method: "POST",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/fork`,
      body: input,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }

  listEvents(
    options: { afterSequence?: number; limit?: number; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<SessionEventPage>> {
    const afterSequence = normalizeEventSequence(options.afterSequence);
    const limit = normalizeEventLimit(options.limit);
    const query = new URLSearchParams({
      afterSequence: String(afterSequence),
      limit: String(limit),
    });
    return this.transport.json<SessionEventPage>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/events?${query.toString()}`,
      signal: options.signal,
    });
  }

  pendingInteractions(
    options: { limit?: number; cursor?: string; signal?: AbortSignal } = {},
  ): Promise<SDKResponse<PendingInteractionPage>> {
    const query = boundedPageQuery(options, "Interaction");
    return this.transport.json<PendingInteractionPage>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(this.id)}/interactions?${query}`,
      signal: options.signal,
    });
  }

  events(options: SessionEventStreamOptions = {}): AsyncGenerator<SessionEvent, void, void> {
    return streamSessionEvents(this.transport, this.id, options);
  }

  async *eventsForExecution(
    executionId: string,
    options: SessionEventStreamOptions = {},
  ): AsyncGenerator<SessionEvent, void, void> {
    if (executionId.trim() === "") throw new TypeError("executionId is required.");
    for await (const event of this.events(options)) {
      if (event.executionId === executionId) yield event;
    }
  }
}

export class ApprovalsResource {
  constructor(private readonly transport: PolarisTransport) {}

  resolve(
    executionId: string,
    requestId: string,
    decision: ApprovalDecision,
    options: IdempotencyOptions = {},
  ): Promise<SDKResponse<Interaction>> {
    return this.transport.json<Interaction>({
      method: "POST",
      path: `/v1/executions/${encodeURIComponent(executionId)}/approvals/${encodeURIComponent(requestId)}/resolve`,
      body: { decision },
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
    });
  }
}

function newIdempotencyKey(): string {
  if (typeof globalThis.crypto?.randomUUID !== "function") {
    throw new PolarisTransportError(
      "Polaris requires crypto.randomUUID() for mutation idempotency.",
    );
  }
  return globalThis.crypto.randomUUID();
}

function provisioningPath(tenantId: string, targetId: string): string {
  return `/v1/tenants/${encodeURIComponent(tenantId)}/execution-targets/${encodeURIComponent(targetId)}/provisioning-operations`;
}

function projectsPath(tenantId: string, organizationId: string): string {
  return `/v1/tenants/${encodeURIComponent(tenantId)}/organizations/${encodeURIComponent(organizationId)}/projects`;
}

function abortableDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted)
    return Promise.reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      clearTimeout(timeout);
      reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
    };
    const timeout = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, milliseconds);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function normalizeEventSequence(value: number | undefined): number {
  if (value === undefined) return 0;
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError("afterSequence must be a non-negative safe integer.");
  }
  return value;
}

function normalizeEventLimit(value: number | undefined): number {
  if (value === undefined) return 50;
  if (!Number.isSafeInteger(value) || value < 1 || value > 200) {
    throw new TypeError("limit must be an integer between 1 and 200.");
  }
  return value;
}

function normalizePageLimit(value: number | undefined, resource: string): number {
  if (value === undefined) return 50;
  if (!Number.isSafeInteger(value) || value < 1 || value > 200) {
    throw new TypeError(`${resource} list limit must be an integer between 1 and 200.`);
  }
  return value;
}

function boundedPageQuery(options: { limit?: number; cursor?: string }, resource: string): string {
  const limit = normalizePageLimit(options.limit, resource);
  const query = new URLSearchParams({ limit: String(limit) });
  if (options.cursor !== undefined) {
    if (options.cursor.trim() === "") throw new TypeError("cursor must not be empty.");
    query.set("cursor", options.cursor);
  }
  return query.toString();
}
