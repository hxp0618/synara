// FILE: provider-host-fixture.ts
// Purpose: Deterministic Provider Host Protocol v2 fixture for Stage 3 acceptance only.

import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  ftruncateSync,
  lstatSync,
  mkdirSync,
  openSync,
  readSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { createInterface } from "node:readline";
import type { Readable } from "node:stream";

import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_HOST_MAX_COMMAND_BYTES,
  PROVIDER_HOST_MAX_MESSAGE_BYTES,
  PROVIDER_HOST_PROTOCOL_VERSION,
  PROVIDER_HOST_PROVIDER_KINDS,
  ProviderHostCommandEnvelope,
  type ProviderCapabilityCatalogEntry,
  type ProviderHostCommandEnvelope as ProviderHostCommand,
  type ProviderHostDescriptor,
  type ProviderHostError,
  type ProviderHostMessageEnvelope,
  type ProviderHostProviderKind,
} from "@synara/contracts/provider-host";
import { PROVIDER_RUNTIME_EVENT_VERSION } from "@synara/contracts";
import { Schema } from "effect";

export const STAGE3_FIXTURE_SCENARIOS = [
  "text",
  "tool",
  "usage",
  "approval",
  "user-input",
  "artifact",
  "workspace-verify",
  "credential",
  "provider-error",
  "steer",
] as const;

export type Stage3FixtureScenario = (typeof STAGE3_FIXTURE_SCENARIOS)[number];
export type Stage3FixtureFault = "none" | "malformed" | "oversized";
export type Stage3FixtureFaultCommand = "Describe" | "SendTurn";

export type Stage3ProviderFixtureOptions = {
  readonly enabledProviders?: ReadonlySet<ProviderHostProviderKind>;
  readonly fault?: Stage3FixtureFault;
  readonly faultCommand?: Stage3FixtureFaultCommand;
  readonly emitLine: (line: string) => void;
};

type CommandRecord = {
  readonly fingerprint: string;
  terminal?: ProviderHostMessageEnvelope;
};

type FixtureSession = {
  readonly provider: "codex" | "claudeAgent";
  readonly workspaceDirectory?: string;
};

type PendingTurnKind = "approval" | "user-input" | "steer";

type PendingTurn = {
  readonly command: ProviderHostCommand;
  readonly kind: PendingTurnKind;
  readonly requestId?: string;
  readonly outputText: string;
  readonly credentialEvidence?: CredentialEvidence;
  readonly workspaceEvidence?: WorkspaceEvidence;
};

type CredentialEvidence = {
  readonly credentialVerified: true;
  readonly credentialPayloadKeys: ReadonlyArray<string>;
};

type WorkspaceEvidence = {
  readonly artifactRelativePath: string;
  readonly artifactContentVerified: true;
};

type CredentialReadResult =
  | { readonly ok: true; readonly evidence: CredentialEvidence }
  | { readonly ok: false; readonly code: "credential_missing" | "credential_invalid" };

const decodeCommand = Schema.decodeUnknownSync(ProviderHostCommandEnvelope);
const FIXTURE_BUILD_VERSION = "stage3-provider-acceptance-fixture-v1";
const FIXTURE_RUNTIME_VERSION = "0.0.0";
const FIXTURE_TIME_ORIGIN = Date.parse("2026-07-14T00:00:00.000Z");
const ARTIFACT_DIRECTORY_NAME = ".synara-stage3-acceptance";
const ARTIFACT_FILE_NAME = "artifact.txt";
const ARTIFACT_RELATIVE_PATH = `${ARTIFACT_DIRECTORY_NAME}/${ARTIFACT_FILE_NAME}`;
const ARTIFACT_CONTENT = "deterministic stage 3 acceptance artifact\n";
export const STAGE3_FIXTURE_CREDENTIAL_SENTINEL =
  "stage3-provider-acceptance-credential-v1";

function openWorkspaceArtifact(workspaceDirectory: string, access: "read" | "write"): number {
  const workspaceRoot = realpathSync.native(workspaceDirectory);
  if (!lstatSync(workspaceRoot).isDirectory()) {
    throw new Error("fixture Workspace is not a directory");
  }

  const artifactDirectory = resolve(workspaceRoot, ARTIFACT_DIRECTORY_NAME);
  if (!isPathWithin(workspaceRoot, artifactDirectory)) {
    throw new Error("fixture artifact directory escapes the Workspace");
  }
  if (access === "write") {
    try {
      mkdirSync(artifactDirectory, { mode: 0o700 });
    } catch (error) {
      if (!hasFileSystemCode(error, "EEXIST")) throw error;
    }
  }

  const directoryStat = lstatSync(artifactDirectory);
  if (directoryStat.isSymbolicLink() || !directoryStat.isDirectory()) {
    throw new Error("fixture artifact directory must not be a symbolic link");
  }
  const resolvedDirectory = realpathSync.native(artifactDirectory);
  if (!isPathWithin(workspaceRoot, resolvedDirectory)) {
    throw new Error("resolved fixture artifact directory escapes the Workspace");
  }

  const artifactPath = resolve(resolvedDirectory, ARTIFACT_FILE_NAME);
  if (!isPathWithin(workspaceRoot, artifactPath)) {
    throw new Error("fixture artifact path escapes the Workspace");
  }

  let flags = (access === "write" ? fsConstants.O_WRONLY : fsConstants.O_RDONLY) | fsConstants.O_NOFOLLOW;
  try {
    const targetStat = lstatSync(artifactPath);
    if (targetStat.isSymbolicLink() || !targetStat.isFile() || targetStat.nlink !== 1) {
      throw new Error("fixture artifact target must be one regular file");
    }
  } catch (error) {
    if (access !== "write" || !hasFileSystemCode(error, "ENOENT")) throw error;
    flags |= fsConstants.O_CREAT | fsConstants.O_EXCL;
  }

  const descriptor = openSync(artifactPath, flags, 0o600);
  try {
    const openedStat = fstatSync(descriptor);
    const visibleStat = lstatSync(artifactPath);
    if (
      !openedStat.isFile() ||
      openedStat.nlink !== 1 ||
      visibleStat.isSymbolicLink() ||
      !sameFile(openedStat, visibleStat)
    ) {
      throw new Error("fixture artifact target changed while it was opened");
    }

    const resolvedArtifact = realpathSync.native(artifactPath);
    if (!isPathWithin(workspaceRoot, resolvedArtifact)) {
      throw new Error("resolved fixture artifact target escapes the Workspace");
    }
    if (!sameFile(openedStat, statSync(resolvedArtifact))) {
      throw new Error("resolved fixture artifact target changed while it was opened");
    }
    return descriptor;
  } catch (error) {
    closeSync(descriptor);
    throw error;
  }
}

function isPathWithin(root: string, candidate: string): boolean {
  const relativePath = relative(root, candidate);
  return (
    relativePath === "" ||
    (relativePath !== ".." && !relativePath.startsWith(`..${sep}`) && !isAbsolute(relativePath))
  );
}

function sameFile(
  left: Pick<ReturnType<typeof fstatSync>, "dev" | "ino">,
  right: Pick<ReturnType<typeof fstatSync>, "dev" | "ino">,
): boolean {
  return left.dev === right.dev && left.ino === right.ino;
}

function hasFileSystemCode(error: unknown, code: string): boolean {
  return error instanceof Error && (error as NodeJS.ErrnoException).code === code;
}

export class Stage3ProviderAcceptanceHost {
  readonly #enabledProviders: ReadonlySet<ProviderHostProviderKind>;
  readonly #fault: Stage3FixtureFault;
  readonly #faultCommand: Stage3FixtureFaultCommand;
  readonly #emitLine: (line: string) => void;
  readonly #commands = new Map<string, CommandRecord>();

  #session: FixtureSession | null = null;
  #pendingTurn: PendingTurn | null = null;
  #messageSequence = 0;
  #turnSequence = 0;
  #faultEmitted = false;
  #credentialReadResult: CredentialReadResult | undefined;

  constructor(options: Stage3ProviderFixtureOptions) {
    this.#enabledProviders = options.enabledProviders ?? new Set();
    this.#fault = options.fault ?? "none";
    this.#faultCommand = options.faultCommand ?? "Describe";
    this.#emitLine = options.emitLine;
  }

  receiveLine(line: string): void {
    if (!line.trim()) return;
    if (Buffer.byteLength(line) > PROVIDER_HOST_MAX_COMMAND_BYTES) {
      this.#emitProtocolInputError(undefined, "Provider Host command exceeds the negotiated size limit.");
      return;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      this.#emitProtocolInputError(undefined, "Provider Host command is not valid JSON.");
      return;
    }

    let command: ProviderHostCommand;
    try {
      command = decodeCommand(parsed);
    } catch {
      this.#emitProtocolInputError(parsed, "Provider Host command does not match the v2 envelope.");
      return;
    }
    this.handleCommand(command);
  }

  handleCommand(command: ProviderHostCommand): void {
    const fingerprint = commandFingerprint(command);
    const previous = this.#commands.get(command.commandId);
    if (previous) {
      if (previous.fingerprint !== fingerprint) {
        this.#emitMessage(
          errorMessage(
            command,
            protocolError("commandId was reused for different command content."),
            this.#nextOccurredAt(),
          ),
        );
        return;
      }
      if (previous.terminal) this.#emitMessage(previous.terminal);
      return;
    }

    this.#commands.set(command.commandId, { fingerprint });
    try {
      this.#executeCommand(command);
    } catch {
      this.#terminalError(
        command,
        errorDetail(
          "internal_error",
          "Stage 3 Provider acceptance fixture failed safely.",
          false,
          true,
          false,
          true,
          true,
        ),
      );
    }
  }

  close(): void {
    if (!this.#pendingTurn) return;
    const pending = this.#pendingTurn;
    this.#pendingTurn = null;
    this.#terminalError(
      pending.command,
      errorDetail(
        "cancelled",
        "Provider Host input closed before the fixture interaction completed.",
        false,
        false,
        false,
        true,
        true,
      ),
    );
  }

  #executeCommand(command: ProviderHostCommand): void {
    if (command.protocolVersion.major !== PROVIDER_HOST_PROTOCOL_VERSION.major) {
      this.#terminalError(
        command,
        errorDetail(
          "provider_version_incompatible",
          `Provider Host Protocol major ${command.protocolVersion.major} is not supported.`,
          false,
          true,
          true,
          true,
          true,
        ),
      );
      return;
    }
    if (containsForbiddenSecretMaterial(command.payload)) {
      this.#terminalError(
        command,
        protocolError("Provider Host commands must not contain Credential, Worker, or Lease Token material."),
      );
      return;
    }
    if (this.#emitConfiguredFault(command)) return;

    switch (command.commandType) {
      case "Describe": {
        const provider = readProvider(command.payload.provider);
        if (!provider) {
          this.#terminalError(command, unknownProviderError());
          return;
        }
        this.#terminalResult(command, {
          descriptor: fixtureDescriptor(provider, this.#enabledProviders),
        });
        return;
      }
      case "StartSession":
      case "ResumeSession": {
        this.#startSession(command);
        return;
      }
      case "SendTurn": {
        this.#sendTurn(command);
        return;
      }
      case "SteerTurn": {
        this.#steerTurn(command);
        return;
      }
      case "InterruptTurn": {
        this.#interruptTurn(command);
        return;
      }
      case "ResolveApproval": {
        this.#resolveInteraction(command, "approval");
        return;
      }
      case "ResolveUserInput": {
        this.#resolveInteraction(command, "user-input");
        return;
      }
      case "StopSession": {
        if (this.#pendingTurn) {
          const pending = this.#pendingTurn;
          this.#pendingTurn = null;
          this.#terminalError(
            pending.command,
            errorDetail("cancelled", "Provider Session was stopped.", false, false, false, true, true),
          );
        }
        this.#session = null;
        this.#terminalResult(command, { stopped: true });
        return;
      }
      case "CompactSession":
      case "RollbackSession":
      case "ForkSession":
      case "StartReview": {
        this.#terminalError(
          command,
          errorDetail(
            "capability_unsupported",
            `${command.commandType} is intentionally unsupported by the Stage 3 acceptance fixture.`,
            false,
            false,
            true,
            true,
            true,
          ),
        );
        return;
      }
    }
  }

  #startSession(command: ProviderHostCommand): void {
    const runnerInput = asRecord(command.payload.runnerInput);
    const workload = asRecord(runnerInput?.workload);
    const provider = readProvider(workload?.provider);
    if (!runnerInput || !workload || !provider) {
      this.#terminalError(command, protocolError("Session command requires runnerInput.workload.provider."));
      return;
    }
    if (provider !== "codex" && provider !== "claudeAgent") {
      this.#terminalError(
        command,
        errorDetail(
          "capability_unsupported",
          `${provider} is Local-only and cannot execute in the remote acceptance fixture.`,
          false,
          false,
          true,
          false,
          false,
        ),
      );
      return;
    }
    if (!this.#enabledProviders.has(provider)) {
      this.#terminalError(
        command,
        errorDetail(
          "capability_unsupported",
          `${provider} is experimental and is not enabled for this acceptance fixture.`,
          false,
          false,
          true,
          true,
          true,
        ),
      );
      return;
    }
    if (!validRuntimeEventVersion(command.payload.runtimeEventVersion)) {
      this.#terminalError(command, protocolError("Session command requested an unsupported Runtime Event version."));
      return;
    }
    if (command.commandType === "ResumeSession" && !hasResumeData(runnerInput, workload)) {
      this.#terminalError(
        command,
        errorDetail(
          "session_resume_invalid",
          "ResumeSession requires a native Cursor or authoritative history.",
          false,
          false,
          false,
          false,
          true,
        ),
      );
      return;
    }

    const workspaceDirectory = optionalString(runnerInput.workspaceDirectory);
    this.#session = { provider, ...(workspaceDirectory ? { workspaceDirectory } : {}) };
    this.#terminalResult(command, {
      provider,
      resumed: command.commandType === "ResumeSession",
    });
  }

  #sendTurn(command: ProviderHostCommand): void {
    if (!this.#session) {
      this.#terminalError(
        command,
        errorDetail(
          "session_resume_invalid",
          "StartSession or ResumeSession must succeed before SendTurn.",
          false,
          false,
          false,
          true,
          true,
        ),
      );
      return;
    }
    if (this.#pendingTurn) {
      this.#terminalError(command, protocolError("Only one SendTurn command may be active in a Provider Session."));
      return;
    }
    if (!validRuntimeEventVersion(command.payload.runtimeEventVersion)) {
      this.#terminalError(command, protocolError("SendTurn requested an unsupported Runtime Event version."));
      return;
    }
    const inputText = optionalString(command.payload.inputText);
    if (!inputText) {
      this.#terminalError(command, protocolError("SendTurn inputText is required."));
      return;
    }

    const scenarios = parseFixtureScenarios(inputText);
    const blocking = scenarios.filter(
      (scenario): scenario is PendingTurnKind =>
        scenario === "approval" || scenario === "user-input" || scenario === "steer",
    );
    if (blocking.length > 1) {
      this.#terminalError(command, protocolError("SendTurn may request only one blocking fixture scenario."));
      return;
    }
    this.#turnSequence += 1;

    if (scenarios.includes("text")) {
      this.#emitRuntimeEvent(command, "content.delta", {
        streamKind: "assistant_text",
        delta: "deterministic fixture text",
      });
    }
    if (scenarios.includes("tool")) {
      this.#emitRuntimeEvent(command, "item.started", {
        itemType: "command_execution",
        status: "inProgress",
        title: "Deterministic fixture tool",
      });
      this.#emitRuntimeEvent(command, "item.completed", {
        itemType: "command_execution",
        status: "completed",
        title: "Deterministic fixture tool",
        data: { exitCode: 0 },
      });
    }
    if (scenarios.includes("usage")) {
      this.#emitRuntimeEvent(command, "thread.token-usage.updated", {
        usage: {
          usedTokens: 42,
          usedPercent: 4.2,
          inputTokens: 24,
          outputTokens: 18,
          toolUses: scenarios.includes("tool") ? 1 : 0,
        },
      });
    }
    if (scenarios.includes("artifact")) {
      const artifact = this.#writeArtifact();
      if (!artifact) {
        this.#terminalError(
          command,
          errorDetail(
            "workspace_invalid",
            "The deterministic fixture artifact could not be written inside the Workspace.",
            false,
            false,
            true,
            true,
            true,
          ),
        );
        return;
      }
      this.#emitPayload(command, "ArtifactCandidate", { artifact });
    }
    let credentialEvidence: CredentialEvidence | undefined;
    if (scenarios.includes("credential")) {
      const credential = this.#readCredentialEvidence();
      if (!credential.ok) {
        this.#terminalError(
          command,
          errorDetail(
            credential.code,
            credential.code === "credential_missing"
              ? "The deterministic fixture Credential FD is unavailable."
              : "The deterministic fixture Credential payload is invalid.",
            false,
            false,
            true,
            true,
            true,
          ),
        );
        return;
      }
      credentialEvidence = credential.evidence;
    }
    let workspaceEvidence: WorkspaceEvidence | undefined;
    if (scenarios.includes("workspace-verify")) {
      workspaceEvidence = this.#verifyWorkspaceArtifact();
      if (!workspaceEvidence) {
        this.#terminalError(
          command,
          errorDetail(
            "workspace_invalid",
            "The deterministic fixture artifact was not preserved inside the Workspace.",
            false,
            false,
            true,
            true,
            true,
          ),
        );
        return;
      }
    }
    if (scenarios.includes("provider-error")) {
      this.#terminalError(
        command,
        errorDetail(
          "provider_rate_limited",
          "Deterministic fixture Provider rate limit.",
          true,
          true,
          false,
          true,
          true,
        ),
      );
      return;
    }

    const outputText = `fixture ${this.#session.provider} turn ${this.#turnSequence} complete`;
    if (blocking[0] === "approval") {
      const requestId = `fixture-approval-${this.#turnSequence}`;
      this.#pendingTurn = {
        command,
        kind: "approval",
        requestId,
        outputText,
        ...(credentialEvidence ? { credentialEvidence } : {}),
        ...(workspaceEvidence ? { workspaceEvidence } : {}),
      };
      this.#emitPayload(command, "InteractionRequest", {
        interactionType: "approval",
        requestId,
        requestKind: "command",
        requestType: "command_execution_approval",
        summary: "Approve the deterministic fixture command",
        args: { command: "fixture-tool" },
      });
      return;
    }
    if (blocking[0] === "user-input") {
      const requestId = `fixture-user-input-${this.#turnSequence}`;
      this.#pendingTurn = {
        command,
        kind: "user-input",
        requestId,
        outputText,
        ...(credentialEvidence ? { credentialEvidence } : {}),
        ...(workspaceEvidence ? { workspaceEvidence } : {}),
      };
      this.#emitPayload(command, "InteractionRequest", {
        interactionType: "user-input",
        requestId,
        questions: [
          {
            id: "fixture-choice",
            header: "Fixture",
            question: "Choose the deterministic acceptance answer.",
            options: [
              { label: "Continue", description: "Complete the fixture turn." },
              { label: "Stop", description: "Decline the fixture turn." },
            ],
            multiSelect: false,
          },
        ],
      });
      return;
    }
    if (blocking[0] === "steer") {
      this.#pendingTurn = {
        command,
        kind: "steer",
        outputText,
        ...(credentialEvidence ? { credentialEvidence } : {}),
        ...(workspaceEvidence ? { workspaceEvidence } : {}),
      };
      this.#emitPayload(command, "Progress", { state: "waiting-for-steer" });
      return;
    }

    this.#terminalResult(command, this.#turnResult(outputText, credentialEvidence, workspaceEvidence));
  }

  #resolveInteraction(command: ProviderHostCommand, expected: "approval" | "user-input"): void {
    const pending = this.#pendingTurn;
    if (!pending || pending.kind !== expected) {
      this.#terminalError(command, protocolError(`${command.commandType} has no matching active interaction.`));
      return;
    }
    const requestId = optionalString(command.payload.requestId);
    if (requestId !== pending.requestId || !asRecord(command.payload.resolution)) {
      this.#terminalError(command, protocolError(`${command.commandType} payload does not match the active interaction.`));
      return;
    }

    this.#pendingTurn = null;
    this.#terminalResult(command, { acknowledged: true, requestId });
    this.#terminalResult(
      pending.command,
      this.#turnResult(pending.outputText, pending.credentialEvidence, pending.workspaceEvidence),
    );
  }

  #steerTurn(command: ProviderHostCommand): void {
    const pending = this.#pendingTurn;
    if (!pending || pending.kind !== "steer") {
      this.#terminalError(command, protocolError("SteerTurn requires an active steer fixture turn."));
      return;
    }
    if (!matchesTargetCommand(command, pending.command.commandId) || !optionalString(command.payload.inputText)) {
      this.#terminalError(command, protocolError("SteerTurn payload does not match the active SendTurn command."));
      return;
    }

    this.#emitRuntimeEvent(pending.command, "content.delta", {
      streamKind: "assistant_text",
      delta: "deterministic fixture steer applied",
    });
    this.#pendingTurn = null;
    this.#terminalResult(command, {
      steered: true,
      targetCommandId: pending.command.commandId,
    });
    this.#terminalResult(
      pending.command,
      this.#turnResult("fixture steer complete", pending.credentialEvidence, pending.workspaceEvidence),
    );
  }

  #interruptTurn(command: ProviderHostCommand): void {
    const pending = this.#pendingTurn;
    if (!pending) {
      this.#terminalError(
        command,
        errorDetail(
          "session_resume_invalid",
          "InterruptTurn requires an active SendTurn command.",
          false,
          false,
          false,
          true,
          true,
        ),
      );
      return;
    }
    if (!matchesTargetCommand(command, pending.command.commandId)) {
      this.#terminalError(command, protocolError("InterruptTurn targetCommandId does not match the active SendTurn command."));
      return;
    }

    this.#pendingTurn = null;
    this.#terminalResult(command, {
      interrupted: true,
      targetCommandId: pending.command.commandId,
      providerResumeCursor: this.#resumeCursor(),
    });
    this.#terminalError(
      pending.command,
      errorDetail("interrupted", "Provider turn was interrupted.", false, false, false, true, true),
    );
  }

  #turnResult(
    outputText: string,
    credentialEvidence?: CredentialEvidence,
    workspaceEvidence?: WorkspaceEvidence,
  ): Record<string, unknown> {
    return {
      output: {
        text: outputText,
        ...(credentialEvidence ? { credentialEvidence } : {}),
        ...(workspaceEvidence ? { workspaceEvidence } : {}),
      },
      providerResumeCursor: this.#resumeCursor(),
    };
  }

  #resumeCursor(): string {
    return `fixture-cursor-${this.#turnSequence}`;
  }

  #writeArtifact(): { path: string; kind: string; originalName: string; contentType: string } | null {
    const workspaceDirectory = this.#session?.workspaceDirectory;
    if (!workspaceDirectory) return null;
    let descriptor: number | undefined;
    try {
      descriptor = openWorkspaceArtifact(workspaceDirectory, "write");
      ftruncateSync(descriptor, 0);
      writeFileSync(descriptor, ARTIFACT_CONTENT, { encoding: "utf8" });
      return {
        path: ARTIFACT_RELATIVE_PATH,
        kind: "generated_file",
        originalName: "artifact.txt",
        contentType: "text/plain",
      };
    } catch {
      return null;
    } finally {
      if (descriptor !== undefined) {
        try {
          closeSync(descriptor);
        } catch {
          // The artifact write has already completed; there is no safe path-based cleanup here.
        }
      }
    }
  }

  #verifyWorkspaceArtifact(): WorkspaceEvidence | undefined {
    const workspaceDirectory = this.#session?.workspaceDirectory;
    if (!workspaceDirectory) return undefined;
    let descriptor: number | undefined;
    try {
      descriptor = openWorkspaceArtifact(workspaceDirectory, "read");
      const expected = Buffer.from(ARTIFACT_CONTENT, "utf8");
      if (fstatSync(descriptor).size !== expected.length) return undefined;
      const actual = Buffer.alloc(expected.length);
      if (readSync(descriptor, actual, 0, actual.length, 0) !== actual.length || !actual.equals(expected)) {
        return undefined;
      }
      return { artifactRelativePath: ARTIFACT_RELATIVE_PATH, artifactContentVerified: true };
    } catch {
      return undefined;
    } finally {
      if (descriptor !== undefined) {
        try {
          closeSync(descriptor);
        } catch {
          // Verification is complete; there is no path-based cleanup to perform.
        }
      }
    }
  }

  #readCredentialEvidence(): CredentialReadResult {
    if (this.#credentialReadResult) return this.#credentialReadResult;
    const descriptor = optionalString(process.env.SYNARA_PROVIDER_CREDENTIAL_FD);
    const fileDescriptor = descriptor ? Number(descriptor) : Number.NaN;
    if (!Number.isInteger(fileDescriptor) || fileDescriptor < 3) {
      this.#credentialReadResult = { ok: false, code: "credential_missing" };
      return this.#credentialReadResult;
    }

    const maximumBytes = 64 * 1024;
    const chunk = Buffer.allocUnsafe(4 * 1024);
    const parts: Buffer[] = [];
    let totalBytes = 0;
    try {
      for (;;) {
        const bytesRead = readSync(fileDescriptor, chunk, 0, chunk.length, null);
        if (bytesRead === 0) break;
        totalBytes += bytesRead;
        if (totalBytes > maximumBytes) {
          for (const part of parts) part.fill(0);
          this.#credentialReadResult = { ok: false, code: "credential_invalid" };
          return this.#credentialReadResult;
        }
        parts.push(Buffer.from(chunk.subarray(0, bytesRead)));
      }
    } catch {
      for (const part of parts) part.fill(0);
      this.#credentialReadResult = { ok: false, code: "credential_invalid" };
      return this.#credentialReadResult;
    } finally {
      chunk.fill(0);
      try {
        closeSync(fileDescriptor);
      } catch {
        // The evidence remains valid even if the inherited descriptor was already closed.
      }
    }

    const encoded = Buffer.concat(parts, totalBytes);
    for (const part of parts) part.fill(0);
    try {
      const decoded = JSON.parse(encoded.toString("utf8"));
      const payload = asRecord(asRecord(decoded)?.payload);
      if (payload?.acceptanceToken !== STAGE3_FIXTURE_CREDENTIAL_SENTINEL) {
        this.#credentialReadResult = { ok: false, code: "credential_invalid" };
      } else {
        this.#credentialReadResult = {
          ok: true,
          evidence: {
            credentialVerified: true,
            credentialPayloadKeys: Object.keys(payload).sort(),
          },
        };
      }
    } catch {
      this.#credentialReadResult = { ok: false, code: "credential_invalid" };
    } finally {
      encoded.fill(0);
    }
    return this.#credentialReadResult;
  }

  #emitRuntimeEvent(command: ProviderHostCommand, eventType: string, payload: Record<string, unknown>): void {
    this.#emitPayload(command, "Event", {
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType,
      payload,
    });
  }

  #emitPayload(
    command: ProviderHostCommand,
    messageType: "Event" | "InteractionRequest" | "ArtifactCandidate" | "Checkpoint" | "Progress",
    payload: Record<string, unknown>,
  ): void {
    this.#emitMessage({
      ...messageBase(command, this.#nextOccurredAt()),
      messageType,
      payload,
    } as ProviderHostMessageEnvelope);
  }

  #terminalResult(command: ProviderHostCommand, payload: Record<string, unknown>): void {
    this.#terminal(command, {
      ...messageBase(command, this.#nextOccurredAt()),
      messageType: "Result",
      payload,
    });
  }

  #terminalError(command: ProviderHostCommand, error: ProviderHostError): void {
    this.#terminal(command, errorMessage(command, error, this.#nextOccurredAt()));
  }

  #terminal(command: ProviderHostCommand, message: ProviderHostMessageEnvelope): void {
    const record = this.#commands.get(command.commandId);
    if (!record || record.terminal) return;
    record.terminal = message;
    this.#emitMessage(message);
  }

  #emitMessage(message: ProviderHostMessageEnvelope): void {
    const encoded = JSON.stringify(message);
    if (Buffer.byteLength(encoded) > PROVIDER_HOST_MAX_MESSAGE_BYTES) {
      throw new Error("fixture message exceeded Provider Host maximumMessageBytes");
    }
    this.#emitLine(encoded);
  }

  #emitConfiguredFault(command: ProviderHostCommand): boolean {
    if (this.#faultEmitted || this.#fault === "none" || command.commandType !== this.#faultCommand) {
      return false;
    }
    this.#faultEmitted = true;
    if (this.#fault === "malformed") {
      this.#emitLine("{malformed-provider-host-jsonl");
    } else {
      this.#emitLine(JSON.stringify({ padding: "x".repeat(PROVIDER_HOST_MAX_MESSAGE_BYTES + 1) }));
    }
    return true;
  }

  #emitProtocolInputError(value: unknown, message: string): void {
    const command = fallbackCommand(value);
    this.#emitMessage(errorMessage(command, protocolError(message), this.#nextOccurredAt()));
  }

  #nextOccurredAt(): string {
    const occurredAt = new Date(FIXTURE_TIME_ORIGIN + this.#messageSequence).toISOString();
    this.#messageSequence += 1;
    return occurredAt;
  }
}

export async function runStage3ProviderAcceptanceFixture(input: {
  readonly source: Readable;
  readonly options: Omit<Stage3ProviderFixtureOptions, "emitLine">;
  readonly emitLine: (line: string) => void;
}): Promise<void> {
  const host = new Stage3ProviderAcceptanceHost({
    ...input.options,
    emitLine: input.emitLine,
  });
  const lines = createInterface({ input: input.source, crlfDelay: Infinity });
  for await (const line of lines) host.receiveLine(line);
  host.close();
}

export function fixtureDescriptor(
  provider: ProviderHostProviderKind,
  enabledProviders: ReadonlySet<ProviderHostProviderKind>,
): ProviderHostDescriptor {
  const entry = catalogEntry(provider);
  const remote = entry.supportTier !== "local-only";
  const version = providerRuntimeVersion(entry);
  return {
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    hostBuildVersion: FIXTURE_BUILD_VERSION,
    capabilityDescriptor: {
      provider,
      supportTier: entry.supportTier,
      adapterVersion: entry.adapterVersion,
      ...(provider === "codex" ? { providerCliVersion: version } : {}),
      runtime: {
        kind: entry.runtimePolicy.kind,
        name: entry.runtimePolicy.name,
        version,
        available: true,
        versionSource: entry.runtimePolicy.versionSource,
        compatibleRange: { ...entry.runtimePolicy.compatibleRange },
        compatible: true,
      },
      releasePolicy: {
        requiresExplicitEnablement: entry.supportTier === "experimental",
        enabled: remote ? enabledProviders.has(provider) : true,
      },
      capabilities: { ...entry.capabilities },
    },
    maximumCommandBytes: PROVIDER_HOST_MAX_COMMAND_BYTES,
    maximumMessageBytes: PROVIDER_HOST_MAX_MESSAGE_BYTES,
    runtimeEventVersions: {
      minimum: PROVIDER_RUNTIME_EVENT_VERSION,
      maximum: PROVIDER_RUNTIME_EVENT_VERSION,
    },
    credentialDeliveryModes: remote ? ["anonymous-fd"] : [],
    resumeStrategies: remote ? ["native-cursor", "authoritative-history"] : [],
  };
}

export function parseFixtureScenarios(inputText: string): ReadonlyArray<Stage3FixtureScenario> {
  const scenarios = STAGE3_FIXTURE_SCENARIOS.filter((scenario) => {
    const escaped = scenario.replace("-", "\\-");
    return new RegExp(`(?:\\[${escaped}\\]|fixture:${escaped}\\b)`, "i").test(inputText);
  });
  return scenarios.length > 0 ? scenarios : ["text"];
}

function catalogEntry(provider: ProviderHostProviderKind): ProviderCapabilityCatalogEntry {
  const entry = PROVIDER_CAPABILITY_CATALOG.providers.find((candidate) => candidate.provider === provider);
  if (!entry) throw new Error("Provider capability catalog is incomplete");
  return entry;
}

function providerRuntimeVersion(entry: ProviderCapabilityCatalogEntry): string {
  if (entry.provider === "codex" || entry.provider === "claudeAgent") {
    return entry.runtimePolicy.compatibleRange.minimumInclusive;
  }
  return FIXTURE_RUNTIME_VERSION;
}

function readProvider(value: unknown): ProviderHostProviderKind | undefined {
  if (typeof value !== "string") return undefined;
  const normalized = value.trim().toLowerCase();
  if (normalized === "claude") return "claudeAgent";
  return PROVIDER_HOST_PROVIDER_KINDS.find((provider) => provider.toLowerCase() === normalized);
}

function validRuntimeEventVersion(value: unknown): boolean {
  return value === undefined || value === PROVIDER_RUNTIME_EVENT_VERSION;
}

function hasResumeData(runnerInput: Record<string, unknown>, workload: Record<string, unknown>): boolean {
  if (optionalString(runnerInput.providerResumeCursor)) return true;
  if (Array.isArray(workload.conversationHistory) && workload.conversationHistory.length > 0) return true;
  return asRecord(workload.resumeSnapshot) !== undefined;
}

function matchesTargetCommand(command: ProviderHostCommand, activeCommandId: string): boolean {
  const target = command.payload.targetCommandId;
  return target === undefined || optionalString(target) === activeCommandId;
}

function commandFingerprint(command: ProviderHostCommand): string {
  return stableStringify({
    executionId: command.executionId,
    generation: command.generation,
    commandType: command.commandType,
    payload: command.payload,
  });
}

function stableStringify(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, item]) => `${JSON.stringify(key)}:${stableStringify(item)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value) ?? "null";
}

function containsForbiddenSecretMaterial(value: unknown): boolean {
  if (Array.isArray(value)) return value.some(containsForbiddenSecretMaterial);
  const record = asRecord(value);
  if (!record) return false;
  for (const [key, item] of Object.entries(record)) {
    const normalized = key.replace(/[^a-z0-9]/gi, "").toLowerCase();
    if (
      normalized === "credential" ||
      normalized === "credentials" ||
      normalized === "apikey" ||
      normalized === "authorization" ||
      normalized === "password" ||
      normalized === "secret" ||
      normalized === "token" ||
      normalized.endsWith("token")
    ) {
      return true;
    }
    if (containsForbiddenSecretMaterial(item)) return true;
  }
  return false;
}

function messageBase(command: ProviderHostCommand, occurredAt: string) {
  return {
    requestId: command.requestId,
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt,
  };
}

function errorMessage(
  command: ProviderHostCommand,
  error: ProviderHostError,
  occurredAt: string,
): ProviderHostMessageEnvelope {
  return {
    ...messageBase(command, occurredAt),
    messageType: "Error",
    error,
  };
}

function errorDetail(
  code: ProviderHostError["code"],
  message: string,
  retryable: boolean,
  requiresNewExecution: boolean,
  requiresUserAction: boolean,
  canReconstructFromHistory: boolean,
  canMoveWorker: boolean,
): ProviderHostError {
  return {
    code,
    message,
    retryable,
    requiresNewExecution,
    requiresUserAction,
    canReconstructFromHistory,
    canMoveWorker,
  };
}

function protocolError(message: string): ProviderHostError {
  return errorDetail("protocol_violation", message, false, true, false, true, true);
}

function unknownProviderError(): ProviderHostError {
  return errorDetail(
    "provider_not_installed",
    "The requested Provider is not known to this acceptance fixture.",
    false,
    false,
    true,
    false,
    false,
  );
}

function fallbackCommand(value: unknown): ProviderHostCommand {
  const candidate = asRecord(value) ?? {};
  return {
    requestId: safeWireString(candidate.requestId, "fixture-protocol-request"),
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: safeWireString(candidate.executionId, "fixture-protocol-execution"),
    generation:
      typeof candidate.generation === "number" && candidate.generation >= 1
        ? Math.floor(candidate.generation)
        : 1,
    commandType: "Describe",
    commandId: safeWireString(candidate.commandId, "fixture-protocol-command") as ProviderHostCommand["commandId"],
    occurredAt: new Date(FIXTURE_TIME_ORIGIN).toISOString(),
    payload: {},
  };
}

function safeWireString(value: unknown, fallback: string): string {
  return typeof value === "string" && value.trim() ? value.trim().slice(0, 200) : fallback;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function enabledProvidersFromEnvironment(value: string | undefined): ReadonlySet<ProviderHostProviderKind> {
  const providers = new Set<ProviderHostProviderKind>();
  for (const token of (value ?? "").split(",")) {
    const provider = readProvider(token);
    if (provider === "codex" || provider === "claudeAgent") providers.add(provider);
  }
  return providers;
}

function parseCliOptions(arguments_: ReadonlyArray<string>): Omit<Stage3ProviderFixtureOptions, "emitLine"> {
  let enabledProviders = enabledProvidersFromEnvironment(
    process.env.SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS,
  );
  let fault: Stage3FixtureFault = "none";
  let faultCommand: Stage3FixtureFaultCommand = "Describe";

  for (const argument of arguments_) {
    if (argument === "--protocol-v2") continue;
    if (argument.startsWith("--enable-providers=")) {
      enabledProviders = enabledProvidersFromEnvironment(argument.slice("--enable-providers=".length));
      continue;
    }
    if (argument.startsWith("--fault=")) {
      const value = argument.slice("--fault=".length);
      if (value !== "none" && value !== "malformed" && value !== "oversized") {
        throw new Error("--fault must be none, malformed, or oversized");
      }
      fault = value;
      continue;
    }
    if (argument.startsWith("--fault-on=")) {
      const value = argument.slice("--fault-on=".length);
      if (value !== "Describe" && value !== "SendTurn") {
        throw new Error("--fault-on must be Describe or SendTurn");
      }
      faultCommand = value;
      continue;
    }
    throw new Error("unsupported Stage 3 Provider acceptance fixture argument");
  }
  return { enabledProviders, fault, faultCommand };
}

async function main(): Promise<void> {
  let options: Omit<Stage3ProviderFixtureOptions, "emitLine">;
  try {
    options = parseCliOptions(process.argv.slice(2));
  } catch {
    process.stderr.write("Stage 3 Provider acceptance fixture received an invalid argument.\n");
    process.exitCode = 2;
    return;
  }
  await runStage3ProviderAcceptanceFixture({
    source: process.stdin,
    options,
    emitLine: (line) => process.stdout.write(`${line}\n`),
  });
}

if (import.meta.main) await main();
