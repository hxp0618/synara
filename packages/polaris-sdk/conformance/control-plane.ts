import { Polaris } from "../src/index";

const baseUrl = requiredEnvironmentVariable("POLARIS_CONFORMANCE_BASE_URL");
const apiKey = requiredEnvironmentVariable("POLARIS_CONFORMANCE_API_KEY");
const executionTargetId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_EXECUTION_TARGET_ID");
const approvalExecutionId = requiredEnvironmentVariable(
  "POLARIS_CONFORMANCE_APPROVAL_EXECUTION_ID",
);
const approvalSessionId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_APPROVAL_SESSION_ID");
const approvalRequestId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_APPROVAL_REQUEST_ID");
const userInputRequestId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_USER_INPUT_REQUEST_ID");
const artifactId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_ARTIFACT_ID");
const targetAPIKey = requiredEnvironmentVariable("POLARIS_CONFORMANCE_TARGET_API_KEY");
const tenantId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_TENANT_ID");
const organizationId = requiredEnvironmentVariable("POLARIS_CONFORMANCE_ORGANIZATION_ID");

const polaris = new Polaris({ apiKey, baseUrl, maxRetries: 0 });
const targetPolaris = new Polaris({ apiKey: targetAPIKey, baseUrl, maxRetries: 0 });
const projectKey = `conformance-project-${crypto.randomUUID()}`;
const projectInput = { name: `SDK conformance ${crypto.randomUUID()}` } as const;
const project = await targetPolaris.projects.create(tenantId, organizationId, projectInput, {
  idempotencyKey: projectKey,
});
const projectReplay = await targetPolaris.projects.create(tenantId, organizationId, projectInput, {
  idempotencyKey: projectKey,
});
const projectPage = await targetPolaris.projects.list(tenantId, organizationId, { limit: 50 });
const projectRead = await targetPolaris.projects.get(project.data.id);
assert(projectReplay.idempotencyReplayed, "Project replay omitted Idempotency-Replayed: true.");
assert(projectRead.data.id === project.data.id, "Project read returned another resource.");
assert(
  projectPage.data.items.some((item) => item.id === project.data.id),
  "Bounded Project list omitted the created Project.",
);
const projectUpdateKey = `conformance-project-update-${crypto.randomUUID()}`;
const updatedProject = await targetPolaris.projects.update(
  project.data.id,
  { name: `${projectInput.name} updated` },
  { idempotencyKey: projectUpdateKey },
);
const updatedProjectReplay = await targetPolaris.projects.update(
  project.data.id,
  { name: `${projectInput.name} updated` },
  { idempotencyKey: projectUpdateKey },
);
assert(updatedProject.data.name.endsWith(" updated"), "Project update was not applied.");
assert(updatedProjectReplay.idempotencyReplayed, "Project update replay was not identified.");
const archiveCandidate = await targetPolaris.projects.create(
  tenantId,
  organizationId,
  { name: `SDK archive conformance ${crypto.randomUUID()}` },
  { idempotencyKey: `conformance-project-archive-create-${crypto.randomUUID()}` },
);
const projectArchiveKey = `conformance-project-archive-${crypto.randomUUID()}`;
await targetPolaris.projects.archive(archiveCandidate.data.id, { idempotencyKey: projectArchiveKey });
const archivedReplay = await targetPolaris.projects.archive(archiveCandidate.data.id, {
  idempotencyKey: projectArchiveKey,
});
assert(archivedReplay.idempotencyReplayed, "Project archive replay was not identified.");
const projectCapabilities = await targetPolaris.projects.capabilities(project.data.id, {
  executionTargetId,
});
assert(projectCapabilities.data.executionTargetId === executionTargetId, "Project capability target drifted.");
assert(projectCapabilities.data.basis === "target", "Project capability basis was not target.");
console.error("[polaris-conformance] created, replayed, listed, read, updated, and archived Projects");
const createKey = `conformance-create-${crypto.randomUUID()}`;
const createInput = {
  projectId: project.data.id,
  title: "Polaris SDK conformance",
  provider: "codex",
  executionTargetId,
} as const;

const session = await polaris.sessions.create(createInput, { idempotencyKey: createKey });
console.error("[polaris-conformance] created Session");
assert(session.id.length > 0, "Session ID was empty.");
assert(session.response.requestId !== null, "Session response omitted X-Request-ID.");
assert(session.response.rateLimit.limit !== null, "Session response omitted RateLimit-Limit.");
assert(!session.response.idempotencyReplayed, "Initial Session create was unexpectedly replayed.");
const sessionCapabilities = await session.capabilities();
assert(sessionCapabilities.data.executionTargetId === executionTargetId, "Session capability target drifted.");
assert(sessionCapabilities.data.basis === "target", "Fresh Session capability basis was not target.");
const sessionUsage = await session.usage();
assert(sessionUsage.data.sessionId === session.id, "Session Usage returned another Session.");
assert(Array.isArray(sessionUsage.data.items), "Session Usage items were not an array.");
const archiveSession = await polaris.sessions.create(
  {
    projectId: project.data.id,
    title: "Polaris SDK archive conformance",
    provider: "codex",
    model: "conformance-model",
    executionTargetId,
  },
  { idempotencyKey: `conformance-archive-session-create-${crypto.randomUUID()}` },
);
const archiveSessionKey = `conformance-session-archive-${crypto.randomUUID()}`;
const switchModelKey = `conformance-session-model-switch-${crypto.randomUUID()}`;
const switchedModel = await archiveSession.switchModel(
  { model: "conformance-model", expectedModel: "conformance-model" },
  { idempotencyKey: switchModelKey },
);
const switchedModelReplay = await archiveSession.switchModel(
  { model: "conformance-model", expectedModel: "conformance-model" },
  { idempotencyKey: switchModelKey },
);
assert(switchedModel.data.model === "conformance-model", "Session model compare-and-switch drifted.");
assert(switchedModelReplay.idempotencyReplayed, "Session model-switch replay was not identified.");
const suspendKey = `conformance-session-suspend-${crypto.randomUUID()}`;
const suspendedSession = await archiveSession.suspend({ idempotencyKey: suspendKey });
const suspendedSessionReplay = await archiveSession.suspend({ idempotencyKey: suspendKey });
assert(suspendedSession.data.status === "suspended", "Session suspend did not change status.");
assert(suspendedSessionReplay.idempotencyReplayed, "Session suspend replay was not identified.");
const resumeKey = `conformance-session-resume-${crypto.randomUUID()}`;
const resumedSession = await archiveSession.resume({ idempotencyKey: resumeKey });
const resumedSessionReplay = await archiveSession.resume({ idempotencyKey: resumeKey });
assert(resumedSession.data.status === "active", "Session resume did not change status.");
assert(resumedSessionReplay.idempotencyReplayed, "Session resume replay was not identified.");
const archivedSession = await archiveSession.archive({ idempotencyKey: archiveSessionKey });
const archivedSessionReplay = await archiveSession.archive({ idempotencyKey: archiveSessionKey });
assert(archivedSession.data.status === "archived", "Session archive did not change status.");
assert(archivedSessionReplay.idempotencyReplayed, "Session archive replay was not identified.");
const cancelSession = await polaris.sessions.create(
  {
    projectId: project.data.id,
    title: "Polaris SDK cancel conformance",
    provider: "codex",
    executionTargetId,
  },
  { idempotencyKey: `conformance-cancel-session-create-${crypto.randomUUID()}` },
);
const cancelTurn = await cancelSession.sendTurn(
  { inputText: "Cancel this queued execution.", runtimeMode: "full-access", interactionMode: "default" },
  { idempotencyKey: `conformance-cancel-turn-create-${crypto.randomUUID()}` },
);
const cancelEvents = await cancelSession.listEvents({ afterSequence: 0, limit: 50 });
const cancellableEvent = cancelEvents.data.items.find((event) => event.executionId !== null);
assert(cancellableEvent?.executionId, "Queued Turn did not emit an Execution identity.");
const cancelKey = `conformance-execution-cancel-${crypto.randomUUID()}`;
const cancelledExecution = await polaris.executions.cancel(cancellableEvent.executionId, {
  idempotencyKey: cancelKey,
});
const cancelledExecutionReplay = await polaris.executions.cancel(cancellableEvent.executionId, {
  idempotencyKey: cancelKey,
});
assert(cancelledExecution.data.status === "cancelled", "Execution cancel did not reach cancelled.");
assert(cancelledExecutionReplay.idempotencyReplayed, "Execution cancel replay was not identified.");
for (const forbidden of ["workerId", "workerManifestId", "providerRuntimeBindingId", "remoteWorkspaceId", "generation"]) {
  assert(!(forbidden in cancelledExecution.data), `Execution projection leaked ${forbidden}.`);
}
const rollbackBoundary = await cancelSession.listEvents({ afterSequence: 0, limit: 50 });
const rollbackKey = `conformance-session-rollback-${crypto.randomUUID()}`;
const rollback = await cancelSession.rollback(
  { expectedLastEventSequence: rollbackBoundary.data.lastSequence, fromTurnId: cancelTurn.data.id },
  { idempotencyKey: rollbackKey },
);
const rollbackReplay = await cancelSession.rollback(
  { expectedLastEventSequence: rollbackBoundary.data.lastSequence, fromTurnId: cancelTurn.data.id },
  { idempotencyKey: rollbackKey },
);
assert(rollback.data.removedTurnCount === 1, "Session rollback did not remove the selected Turn.");
assert(rollbackReplay.idempotencyReplayed, "Session rollback replay was not identified.");
const forkKey = `conformance-session-fork-${crypto.randomUUID()}`;
const forked = await cancelSession.fork(
  { expectedLastEventSequence: rollback.data.eventSequence, title: "SDK conformance fork", visibility: "organization" },
  { idempotencyKey: forkKey },
);
const forkedReplay = await cancelSession.fork(
  { expectedLastEventSequence: rollback.data.eventSequence, title: "SDK conformance fork", visibility: "organization" },
  { idempotencyKey: forkKey },
);
assert(forked.data.session.id !== cancelSession.id, "Session fork reused the source ID.");
assert(forkedReplay.idempotencyReplayed, "Session fork replay was not identified.");
const interruptSession = await polaris.sessions.create(
  {
    projectId: project.data.id,
    title: "Polaris SDK interrupt conformance",
    provider: "codex",
    executionTargetId,
  },
  { idempotencyKey: `conformance-interrupt-session-create-${crypto.randomUUID()}` },
);
await interruptSession.sendTurn(
  { inputText: "Interrupt this queued execution.", runtimeMode: "full-access", interactionMode: "default" },
  { idempotencyKey: `conformance-interrupt-turn-create-${crypto.randomUUID()}` },
);
const interruptKey = `conformance-turn-interrupt-${crypto.randomUUID()}`;
const interruptCommand = await interruptSession.interrupt({ idempotencyKey: interruptKey });
const interruptCommandReplay = await interruptSession.interrupt({ idempotencyKey: interruptKey });
assert(interruptCommand.data.commandType === "InterruptTurn", "Interrupt returned another command type.");
assert(interruptCommandReplay.idempotencyReplayed, "Interrupt replay was not identified.");
for (const forbidden of ["payload", "deliveryWorkerId", "deliveryGeneration", "deliveryAttempts", "deliveryError"]) {
  assert(!(forbidden in interruptCommand.data), `Control command projection leaked ${forbidden}.`);
}

const replayed = await polaris.sessions.create(createInput, { idempotencyKey: createKey });
console.error("[polaris-conformance] replayed Session mutation");
assert(replayed.id === session.id, "Idempotent Session replay returned another resource.");
assert(replayed.response.idempotencyReplayed, "Session replay omitted Idempotency-Replayed: true.");
const sessionPage = await polaris.sessions.list(project.data.id, { limit: 50 });
const sessionRead = await polaris.sessions.get(session.id);
assert(
  sessionPage.data.items.some((item) => item.id === session.id),
  "Bounded Session list omitted the created Session.",
);
assert(sessionRead.id === session.id, "Session read returned another resource.");
console.error("[polaris-conformance] listed and read Session");

const turnResponse = await session.sendTurn(
  {
    inputText: "Verify the public SDK against the real Control Plane.",
    runtimeMode: "full-access",
    interactionMode: "default",
  },
  { idempotencyKey: `conformance-turn-${crypto.randomUUID()}` },
);
console.error("[polaris-conformance] created Turn");
assert(turnResponse.data.sessionId === session.id, "Turn response belonged to another Session.");

const eventPage = await session.listEvents({ afterSequence: 0, limit: 50 });
assert(eventPage.data.items.length >= 2, "Event list did not return the durable Session backlog.");
assert(
  eventPage.data.lastSequence >= eventPage.data.items.at(-1)!.sequence,
  "Event list watermark preceded its final item.",
);
console.error("[polaris-conformance] listed durable events");

const observedSequences: number[] = [];
const observedTypes: string[] = [];
const streamAbort = new AbortController();
const streamTimeout = setTimeout(
  () => streamAbort.abort(new Error("SSE conformance timed out.")),
  5_000,
);
try {
  for await (const event of session.events({ reconnect: false, signal: streamAbort.signal })) {
    observedSequences.push(event.sequence);
    observedTypes.push(event.eventType);
    if (event.eventType === "turn.created") break;
  }
} catch (error) {
  if (!streamAbort.signal.aborted) throw error;
} finally {
  clearTimeout(streamTimeout);
}
console.error(`[polaris-conformance] consumed SSE events: ${observedTypes.join(", ")}`);
assert(
  observedTypes.includes("session.created"),
  `SSE replay omitted session.created; observed ${JSON.stringify(observedTypes)}.`,
);
assert(
  observedTypes.includes("turn.created"),
  `SSE replay omitted turn.created; observed ${JSON.stringify(observedTypes)}.`,
);
assert(
  observedSequences.every(
    (sequence, index) => index === 0 || sequence === observedSequences[index - 1]! + 1,
  ),
  "SSE replay was not contiguous.",
);

await new Promise((resolve) => setTimeout(resolve, 50));
const heldStreamAbort = new AbortController();
let heldStream: Response | undefined;
for (let attempt = 0; attempt < 20; attempt += 1) {
  const response = await fetch(
    `${baseUrl}/v1/sessions/${encodeURIComponent(session.id)}/events/stream?afterSequence=0`,
    {
      headers: { Authorization: `Bearer ${apiKey}`, Accept: "text/event-stream" },
      signal: heldStreamAbort.signal,
    },
  );
  if (response.ok) {
    heldStream = response;
    break;
  }
  await response.body?.cancel().catch(() => undefined);
  await new Promise((resolve) => setTimeout(resolve, 50));
}
assert(heldStream?.ok, "Could not reserve the SSE connection pool after the prior stream closed.");
const fallbackTypes: string[] = [];
try {
  for await (const event of session.events({
    reconnect: false,
    pollingIntervalMs: 0,
    pollingLimit: 50,
  })) {
    fallbackTypes.push(event.eventType);
    if (event.eventType === "turn.created") break;
  }
} finally {
  heldStreamAbort.abort();
  await heldStream.body?.cancel().catch(() => undefined);
}
assert(
  fallbackTypes.includes("turn.created"),
  "SSE connection-limit polling fallback missed Turn events.",
);
console.error(`[polaris-conformance] verified polling fallback: ${fallbackTypes.join(", ")}`);

const approvalKey = `conformance-approval-${crypto.randomUUID()}`;
const approvalSession = await polaris.sessions.get(approvalSessionId);
const artifactPage = await polaris.artifacts.list(approvalSessionId, { limit: 50 });
const artifactRead = await polaris.artifacts.get(artifactId);
const artifactDownload = await polaris.artifacts.download(artifactId);
assert(
  artifactPage.data.items.some((item) => item.id === artifactId),
  "Artifact metadata page omitted the seeded Artifact.",
);
assert(artifactRead.data.id === artifactId, "Artifact metadata read returned another resource.");
assert(
  artifactDownload.data.artifact.id === artifactId && artifactDownload.data.url.length > 0,
  "Artifact download grant was invalid.",
);
assert(
  !JSON.stringify(artifactRead.data).includes("objectKey"),
  "Artifact metadata leaked an object key.",
);
console.error("[polaris-conformance] listed and read payload-free Artifact metadata");
const artifactPayload = new TextEncoder().encode("Polaris Artifact conformance\n");
const artifactDigest = Array.from(
  new Uint8Array(await crypto.subtle.digest("SHA-256", artifactPayload)),
)
  .map((value) => value.toString(16).padStart(2, "0"))
  .join("");
const artifactCreateKey = `conformance-artifact-${crypto.randomUUID()}`;
const artifactInput = { kind: "generated_file", originalName: "polaris-conformance.txt" } as const;
const artifactCreated = await polaris.artifacts.create(approvalSessionId, artifactInput, {
  idempotencyKey: artifactCreateKey,
});
const artifactCreateReplay = await polaris.artifacts.create(approvalSessionId, artifactInput, {
  idempotencyKey: artifactCreateKey,
});
assert(
  artifactCreateReplay.data.artifact.id === artifactCreated.data.artifact.id &&
    artifactCreateReplay.idempotencyReplayed,
  "Artifact create replay did not retain identity and rotate its grant.",
);
assert(
  artifactCreateReplay.data.uploadRequired,
  "Pending Artifact replay omitted its upload grant.",
);
const staleUploadResponse = await fetch(new URL(artifactCreated.data.url!, baseUrl), {
  method: "PUT",
  headers: { ...artifactCreated.data.headers, "Content-Type": "text/plain" },
  body: artifactPayload,
});
assert(
  staleUploadResponse.status === 401,
  "Artifact replay did not revoke the previous local upload token.",
);
await staleUploadResponse.body?.cancel().catch(() => undefined);
const uploadResponse = await fetch(new URL(artifactCreateReplay.data.url!, baseUrl), {
  method: "PUT",
  headers: { ...artifactCreateReplay.data.headers, "Content-Type": "text/plain" },
  body: artifactPayload,
});
assert(uploadResponse.ok, `Artifact upload failed with HTTP ${uploadResponse.status}.`);
await uploadResponse.body?.cancel().catch(() => undefined);
const completedArtifact = await polaris.artifacts.complete(
  artifactCreated.data.artifact.id,
  { sizeBytes: artifactPayload.byteLength, sha256: artifactDigest, contentType: "text/plain" },
  { idempotencyKey: `${artifactCreateKey}:complete` },
);
const completedArtifactReplay = await polaris.artifacts.complete(
  artifactCreated.data.artifact.id,
  { sizeBytes: artifactPayload.byteLength, sha256: artifactDigest, contentType: "text/plain" },
  { idempotencyKey: `${artifactCreateKey}:complete` },
);
assert(
  completedArtifact.data.status === "ready" &&
    completedArtifactReplay.data.id === completedArtifact.data.id,
  "Artifact completion was not intrinsically replay-safe.",
);
await polaris.artifacts.delete(completedArtifact.data.id, {
  idempotencyKey: `${artifactCreateKey}:delete`,
});
await polaris.artifacts.delete(completedArtifact.data.id, {
  idempotencyKey: `${artifactCreateKey}:delete`,
});
console.error("[polaris-conformance] created, replayed, uploaded, and completed Artifact");
const pendingInteractions = await approvalSession.pendingInteractions({ limit: 50 });
const interactionHistory = await polaris.interactions.list(approvalExecutionId, { limit: 50 });
assert(
  pendingInteractions.data.items.some((item) => item.requestId === approvalRequestId),
  "Pending Interaction page omitted the seeded approval.",
);
assert(
  interactionHistory.data.items.some((item) => item.requestId === approvalRequestId),
  "Interaction history page omitted the seeded approval.",
);
assert(
  !JSON.stringify(interactionHistory.data).includes("deliveryStatus"),
  "Developer Interaction history leaked Worker delivery internals.",
);
console.error("[polaris-conformance] read bounded pending and historical Interactions");
const approval = await polaris.approvals.resolve(approvalExecutionId, approvalRequestId, "accept", {
  idempotencyKey: approvalKey,
});
console.error("[polaris-conformance] resolved approval");
assert(approval.data.status === "resolved", "Approval did not resolve.");
assert(
  approval.data.resolution?.decision === "accept",
  "Approval resolution payload was incorrect.",
);

const approvalReplay = await polaris.approvals.resolve(
  approvalExecutionId,
  approvalRequestId,
  "accept",
  { idempotencyKey: approvalKey },
);
console.error("[polaris-conformance] replayed approval mutation");
assert(approvalReplay.idempotencyReplayed, "Approval replay omitted Idempotency-Replayed: true.");
const userInputKey = `conformance-user-input-${crypto.randomUUID()}`;
const userInput = await polaris.interactions.resolveUserInput(
  approvalExecutionId,
  userInputRequestId,
  { environment: "staging" },
  { idempotencyKey: userInputKey },
);
const userInputReplay = await polaris.interactions.resolveUserInput(
  approvalExecutionId,
  userInputRequestId,
  { environment: "staging" },
  { idempotencyKey: userInputKey },
);
assert(userInput.data.status === "resolved", "Structured user input did not resolve.");
assert(
  userInputReplay.idempotencyReplayed,
  "User-input replay omitted Idempotency-Replayed: true.",
);
console.error("[polaris-conformance] resolved and replayed structured user input");

const targetKey = `conformance-target-${crypto.randomUUID()}`;
const targetInput = {
  organizationId,
  kind: "ssh",
  name: `SDK conformance ${crypto.randomUUID()}`,
  configuration: {},
  capabilities: {},
} as const;
const target = await targetPolaris.targets.create(tenantId, targetInput, {
  idempotencyKey: targetKey,
});
const targetReplay = await targetPolaris.targets.create(tenantId, targetInput, {
  idempotencyKey: targetKey,
});
const targetRead = await targetPolaris.targets.get(tenantId, target.data.id);
assert(targetReplay.idempotencyReplayed, "Execution Target replay omitted Idempotency-Replayed.");
assert(targetRead.data.id === target.data.id, "Execution Target read returned another resource.");
assert(!("configuration" in targetRead.data), "Execution Target projection leaked configuration.");
console.error("[polaris-conformance] registered and read BYO Execution Target");

const provisioning = await targetPolaris.targets.provision(tenantId, target.data.id, "install", {
  idempotencyKey: `conformance-provision-${crypto.randomUUID()}`,
});
const provisioned = await targetPolaris.targets.waitForProvisioning(
  tenantId,
  target.data.id,
  provisioning.data.id,
  { pollIntervalMs: 10, timeoutMs: 5_000 },
);
assert(provisioned.data.state === "succeeded", "Execution Target provisioning did not succeed.");
assert(
  !JSON.stringify(provisioned.data).includes("configuration"),
  "Provisioning projection leaked configuration.",
);
console.error("[polaris-conformance] created and polled durable provisioning operation");

process.stdout.write(
  `${JSON.stringify({
    sessionId: session.id,
    projectId: project.data.id,
    archivedProjectId: archiveCandidate.data.id,
    archivedSessionId: archivedSession.data.id,
    cancelledExecutionId: cancelledExecution.data.id,
    rollbackEventId: rollback.data.eventId,
    forkedSessionId: forked.data.session.id,
    interruptedExecutionId: interruptCommand.data.executionId,
    turnId: turnResponse.data.id,
    eventTypes: observedTypes,
    approvalId: approval.data.id,
    targetId: target.data.id,
    provisioningOperationId: provisioning.data.id,
    createdArtifactId: completedArtifact.data.id,
  })}\n`,
);

function requiredEnvironmentVariable(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required.`);
  return value;
}

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}
