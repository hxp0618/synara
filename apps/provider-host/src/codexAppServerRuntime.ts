import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createInterface } from "node:readline";

import {
  hasAuthoritativeResumeData,
  type ProviderRunController,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { ProviderInterruptedError } from "./providerRunErrors";

type JsonRpcId = string | number;

type JsonRpcRequest = {
  id: JsonRpcId;
  method: string;
  params?: unknown;
};

type JsonRpcResponse = {
  id: JsonRpcId;
  result?: unknown;
  error?: { code?: number; message?: string };
};

type JsonRpcNotification = {
  method: string;
  params?: unknown;
};

type PendingRequest = {
  method: string;
  timeout: NodeJS.Timeout;
  resolve: (value: unknown) => void;
  reject: (error: Error) => void;
};

type PendingInteraction = {
  jsonRpcId: JsonRpcId;
};

type CodexRunOptions = {
  input: RunnerInput;
  environment: NodeJS.ProcessEnv;
  redact: (value: string) => string;
  emit: (message: RunnerMessage) => void;
  authoritativePrompt: string;
  interactive: boolean;
};

const REQUEST_TIMEOUT_MS = 20_000;
const INTERRUPT_GRACE_MS = 2_000;
const MAX_STDERR_BYTES = 64 * 1024;
const MAX_WIRE_LINE_BYTES = 4 * 1024 * 1024;

export function startCodexAppServerRun(options: CodexRunOptions): ProviderRunController {
  const runtime = new CodexAppServerRuntime(options);
  return runtime.start();
}

class CodexAppServerRuntime {
  private readonly child: ChildProcessWithoutNullStreams;
  private readonly pendingRequests = new Map<string, PendingRequest>();
  private readonly pendingApprovals = new Map<string, PendingInteraction>();
  private readonly pendingUserInputs = new Map<string, PendingInteraction>();
  private readonly outputText: string[] = [];
  private readonly turnCompletion: Promise<Record<string, unknown>>;
  private resolveTurn!: (turn: Record<string, unknown>) => void;
  private rejectTurn!: (error: Error) => void;
  private nextRequestId = 1;
  private threadId: string | undefined;
  private turnId: string | undefined;
  private model: string | undefined;
  private stderr = "";
  private lastError: string | undefined;
  private interruptRequested = false;
  private processExited = false;
  private turnSettled = false;
  private forceKillTimer: NodeJS.Timeout | undefined;

  constructor(private readonly options: CodexRunOptions) {
    this.child = spawn("codex", ["app-server"], {
      cwd: options.input.workspaceDirectory,
      env: options.environment,
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.turnCompletion = new Promise<Record<string, unknown>>((resolve, reject) => {
      this.resolveTurn = resolve;
      this.rejectTurn = reject;
    });
    // Process startup can fail before run() reaches the turn wait. Register a
    // rejection handler immediately so that failure never becomes unhandled.
    void this.turnCompletion.catch(() => {});
    this.attachProcessListeners();
  }

  start(): ProviderRunController {
    return {
      result: this.run(),
      interrupt: () => this.interrupt(),
      getResumeCursor: () => this.threadId,
      steer: (payload) => this.steer(payload),
      resolveApproval: (payload) => this.resolveApproval(payload),
      resolveUserInput: (payload) => this.resolveUserInput(payload),
    };
  }

  private async run(): Promise<Extract<RunnerMessage, { type: "result" }>> {
    try {
      await this.sendRequest("initialize", {
        clientInfo: {
          name: "synara_desktop",
          title: "Synara Provider Host",
          version: "0.2.0",
        },
        capabilities: {
          experimentalApi: true,
          requestAttestation: false,
        },
      });
      this.writeMessage({ method: "initialized", params: {} });

      const resumed = await this.openThread();
      const prompt = resumed ? this.options.input.workload.inputText : this.options.authoritativePrompt;
      const turnParams = {
        threadId: this.threadId,
        input: [{ type: "text", text: prompt, text_elements: [] }],
        ...(trimmedString(this.options.input.workload.model)
          ? { model: trimmedString(this.options.input.workload.model) }
          : {}),
        ...(this.options.interactive && this.options.input.workload.interactionMode === "plan"
          ? { collaborationMode: this.planCollaborationMode() }
          : {}),
      };
      const response = asRecord(
        await this.sendRequest("turn/start", {
          ...turnParams,
        }),
      );
      this.turnId = readString(asRecord(response.turn), "id");
      if (!this.turnId) {
        throw new Error("Codex app-server turn/start response did not include a turn id.");
      }
      if (this.interruptRequested) this.requestNativeInterrupt();

      const completedTurn = await this.turnCompletion;
      if (this.outputText.length === 0) {
        const finalText = finalAgentText(completedTurn);
        if (finalText) this.outputText.push(this.options.redact(finalText));
      }
      return {
        type: "result",
        output: {
          provider: "codex",
          model: this.model ?? this.options.input.workload.model ?? null,
          text: this.outputText.join(""),
        },
        ...(this.threadId ? { providerResumeCursor: this.threadId } : {}),
      };
    } finally {
      this.terminateProcess();
    }
  }

  private async openThread(): Promise<boolean> {
    const cursor = trimmedString(this.options.input.providerResumeCursor);
    const historyAvailable = hasAuthoritativeResumeData(this.options.input.workload);
    const approvalRequired =
      this.options.interactive && this.options.input.workload.runtimeMode === "approval-required";
    const common = {
      ...(trimmedString(this.options.input.workload.model)
        ? { model: trimmedString(this.options.input.workload.model) }
        : {}),
      cwd: this.options.input.workspaceDirectory,
      approvalPolicy: approvalRequired ? "untrusted" : "never",
      approvalsReviewer: "user",
      sandbox: approvalRequired ? "read-only" : "danger-full-access",
    } as const;

    if (cursor) {
      try {
        const response = asRecord(
          await this.sendRequest("thread/resume", {
            ...common,
            threadId: cursor,
          }),
        );
        this.threadId = readThreadId(response);
        this.model = readString(response, "model") ?? trimmedString(this.options.input.workload.model);
        if (!this.threadId) {
          throw new Error("Codex app-server thread/resume response did not include a thread id.");
        }
        return true;
      } catch (error) {
        if (!historyAvailable) {
          const detail = error instanceof Error ? error.message : String(error);
          throw new Error(`Codex app-server session resume is invalid: ${detail}`);
        }
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.warning",
          payload: {
            provider: "codex",
            message: "Native Codex resume was unavailable; rebuilt the turn from authoritative history.",
          },
        });
      }
    }

    const response = asRecord(await this.sendRequest("thread/start", common));
    this.threadId = readThreadId(response);
    this.model = readString(response, "model") ?? trimmedString(this.options.input.workload.model);
    if (!this.threadId) {
      throw new Error("Codex app-server thread/start response did not include a thread id.");
    }
    return false;
  }

  private attachProcessListeners(): void {
    const output = createInterface({ input: this.child.stdout, crlfDelay: Infinity });
    output.on("line", (line) => this.handleLine(line));

    this.child.stderr.setEncoding("utf8");
    this.child.stderr.on("data", (chunk: string) => {
      this.stderr = (this.stderr + this.options.redact(chunk)).slice(-MAX_STDERR_BYTES);
    });
    this.child.stdin.on("error", () => {
      // Early exits are reported through the process error/close listeners.
    });
    this.child.once("error", (error) => this.failRuntime(error));
    this.child.once("close", (code, signal) => {
      this.processExited = true;
      if (this.forceKillTimer) clearTimeout(this.forceKillTimer);
      if (this.turnSettled) return;
      if (this.interruptRequested) {
        this.failRuntime(new ProviderInterruptedError());
        return;
      }
      const detail = this.stderr.trim();
      this.failRuntime(
        new Error(
          detail ||
            `Codex app-server exited before turn completion (code=${code ?? "null"}, signal=${signal ?? "null"}).`,
        ),
      );
    });
  }

  private planCollaborationMode(): Record<string, unknown> {
    if (!this.model) {
      throw new Error("Codex app-server did not report a model required for Plan Mode.");
    }
    return {
      mode: "plan",
      settings: {
        model: this.model,
        reasoning_effort: "medium",
        developer_instructions: null,
      },
    };
  }

  private handleLine(line: string): void {
    if (!line.trim()) return;
    if (Buffer.byteLength(line) > MAX_WIRE_LINE_BYTES) {
      this.failRuntime(new Error("Codex app-server emitted an oversized JSONL message."));
      return;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      this.failRuntime(new Error("Codex app-server emitted invalid JSONL."));
      return;
    }
    const message = asRecord(parsed);
    if (!message) {
      this.failRuntime(new Error("Codex app-server emitted a non-object JSON-RPC message."));
      return;
    }
    const method = readString(message, "method");
    const hasId = Object.prototype.hasOwnProperty.call(message, "id");
    if (method && hasId) {
      this.handleServerRequest(message as JsonRpcRequest);
      return;
    }
    if (method) {
      this.handleNotification(message as JsonRpcNotification);
      return;
    }
    if (hasId && ("result" in message || "error" in message)) {
      this.handleResponse(message as JsonRpcResponse);
      return;
    }
    this.failRuntime(new Error("Codex app-server emitted an unrecognized JSON-RPC envelope."));
  }

  private handleServerRequest(request: JsonRpcRequest): void {
    const params = asRecord(request.params) ?? {};
    const requestId = interactionRequestId(request.id);
    if (
      request.method === "item/commandExecution/requestApproval" ||
      request.method === "item/fileChange/requestApproval" ||
      request.method === "item/fileRead/requestApproval"
    ) {
      if (this.pendingApprovals.has(requestId)) {
        this.failRuntime(new Error(`Codex app-server reused approval request ${requestId}.`));
        return;
      }
      this.pendingApprovals.set(requestId, { jsonRpcId: request.id });
      this.options.emit({
        type: "interaction",
        interactionType: "approval",
        payload: approvalPayload(request.method, requestId, params),
      });
      return;
    }
    if (request.method === "item/tool/requestUserInput") {
      if (this.pendingUserInputs.has(requestId)) {
        this.failRuntime(new Error(`Codex app-server reused user-input request ${requestId}.`));
        return;
      }
      this.pendingUserInputs.set(requestId, { jsonRpcId: request.id });
      this.options.emit({
        type: "interaction",
        interactionType: "user-input",
        payload: userInputPayload(requestId, params),
      });
      return;
    }

    this.writeMessage({
      id: request.id,
      error: { code: -32601, message: `Unsupported Codex app-server request: ${request.method}` },
    });
  }

  private handleNotification(notification: JsonRpcNotification): void {
    const params = asRecord(notification.params) ?? {};
    switch (notification.method) {
      case "thread/started": {
        this.threadId = readString(asRecord(params.thread), "id") ?? this.threadId;
        return;
      }
      case "item/agentMessage/delta": {
        const delta = readString(params, "delta");
        if (!delta) return;
        const text = this.options.redact(delta);
        this.outputText.push(text);
        this.options.emit({
          type: "event",
          eventType: "runtime.output.delta",
          payload: { text },
        });
        return;
      }
      case "item/started":
      case "item/completed": {
        const item = asRecord(params.item);
        const itemType = readString(item, "type");
        if (!itemType || itemType === "agentMessage" || itemType === "userMessage") return;
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: {
            provider: "codex",
            itemType,
            status: notification.method === "item/started" ? "started" : "completed",
            ...(readString(item, "id") ? { itemId: readString(item, "id") } : {}),
          },
        });
        return;
      }
      case "turn/diff/updated":
      case "turn/plan/updated": {
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: {
            provider: "codex",
            itemType: notification.method === "turn/diff/updated" ? "diff" : "plan",
            status: "updated",
          },
        });
        return;
      }
      case "thread/tokenUsage/updated": {
        const usage = asRecord(asRecord(params.tokenUsage)?.last);
        if (!usage) return;
        this.options.emit({
          type: "event",
          eventType: "runtime.usage",
          payload: { provider: "codex", ...numericFields(usage) },
        });
        return;
      }
      case "warning":
      case "error": {
        const message =
          readString(asRecord(params.error), "message") ??
          readString(params, "message") ??
          "Codex app-server reported a warning.";
        const redacted = this.options.redact(message);
        this.lastError = redacted;
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.warning",
          payload: { provider: "codex", message: redacted },
        });
        return;
      }
      case "serverRequest/resolved": {
        const resolvedId = params.requestId;
        if (typeof resolvedId === "string" || typeof resolvedId === "number") {
          const requestId = interactionRequestId(resolvedId);
          this.pendingApprovals.delete(requestId);
          this.pendingUserInputs.delete(requestId);
        }
        return;
      }
      case "turn/aborted": {
        this.settleTurn(new ProviderInterruptedError());
        return;
      }
      case "turn/completed": {
        const turn = asRecord(params.turn);
        const completedTurnId = readString(turn, "id");
        if (this.turnId && completedTurnId && completedTurnId !== this.turnId) return;
        const status = readString(turn, "status");
        if (status === "completed") {
          this.settleTurn(undefined, turn ?? {});
        } else if (status === "interrupted") {
          this.settleTurn(new ProviderInterruptedError());
        } else {
          const message =
            readString(asRecord(turn?.error), "message") ??
            this.lastError ??
            "Codex app-server turn failed.";
          this.settleTurn(new Error(this.options.redact(message)));
        }
        return;
      }
      default:
        return;
    }
  }

  private handleResponse(response: JsonRpcResponse): void {
    const pending = this.pendingRequests.get(String(response.id));
    if (!pending) return;
    clearTimeout(pending.timeout);
    this.pendingRequests.delete(String(response.id));
    if (response.error) {
      pending.reject(
        new Error(
          `${pending.method} failed: ${this.options.redact(response.error.message ?? "Unknown JSON-RPC error")}`,
        ),
      );
      return;
    }
    pending.resolve(response.result);
  }

  private resolveApproval(payload: Record<string, unknown>): void {
    const requestId = requiredString(payload.requestId, "ResolveApproval requestId");
    const pending = this.pendingApprovals.get(requestId);
    if (!pending) throw new Error(`Unknown pending Codex approval request: ${requestId}`);
    const resolution = asRecord(payload.resolution);
    const decision = readString(resolution, "decision");
    if (decision !== "accept" && decision !== "decline") {
      throw new Error("Codex approval resolution must be accept or decline.");
    }
    this.pendingApprovals.delete(requestId);
    this.writeMessage({ id: pending.jsonRpcId, result: { decision } });
  }

  private resolveUserInput(payload: Record<string, unknown>): void {
    const requestId = requiredString(payload.requestId, "ResolveUserInput requestId");
    const pending = this.pendingUserInputs.get(requestId);
    if (!pending) throw new Error(`Unknown pending Codex user-input request: ${requestId}`);
    const resolution = asRecord(payload.resolution);
    const answers = asRecord(resolution?.answers);
    if (!answers) throw new Error("Codex user-input resolution must include an answers object.");
    this.pendingUserInputs.delete(requestId);
    this.writeMessage({ id: pending.jsonRpcId, result: { answers: codexUserInputAnswers(answers) } });
  }

  private async steer(payload: Record<string, unknown>): Promise<void> {
    const inputText = requiredString(payload.inputText, "SteerTurn inputText");
    if (!this.threadId || !this.turnId || this.turnSettled || this.processExited) {
      throw new Error("Codex app-server does not have an active steerable Turn.");
    }
    await this.sendRequest("turn/steer", {
      threadId: this.threadId,
      expectedTurnId: this.turnId,
      input: [{ type: "text", text: inputText, text_elements: [] }],
    });
  }

  private interrupt(): void {
    if (this.turnSettled || this.processExited || this.interruptRequested) return;
    this.interruptRequested = true;
    if (this.threadId && this.turnId) {
      this.requestNativeInterrupt();
    } else {
      this.failRuntime(new ProviderInterruptedError());
    }
  }

  private requestNativeInterrupt(): void {
    if (!this.threadId || !this.turnId || this.turnSettled || this.processExited) return;
    void this.sendRequest(
      "turn/interrupt",
      { threadId: this.threadId, turnId: this.turnId },
      INTERRUPT_GRACE_MS,
    ).catch(() => this.terminateProcess());
    this.scheduleForceKill();
  }

  private sendRequest(method: string, params: unknown, timeoutMs = REQUEST_TIMEOUT_MS): Promise<unknown> {
    const id = this.nextRequestId++;
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pendingRequests.delete(String(id));
        reject(new Error(`Timed out waiting for Codex app-server ${method}.`));
      }, timeoutMs);
      this.pendingRequests.set(String(id), { method, timeout, resolve, reject });
      try {
        this.writeMessage({ method, id, params });
      } catch (error) {
        clearTimeout(timeout);
        this.pendingRequests.delete(String(id));
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    });
  }

  private writeMessage(message: unknown): void {
    if (!this.child.stdin.writable) throw new Error("Codex app-server stdin is not writable.");
    this.child.stdin.write(`${JSON.stringify(message)}\n`);
  }

  private settleTurn(error?: Error, turn: Record<string, unknown> = {}): void {
    if (this.turnSettled) return;
    this.turnSettled = true;
    if (this.forceKillTimer) clearTimeout(this.forceKillTimer);
    if (error) this.rejectTurn(error);
    else this.resolveTurn(turn);
  }

  private failRuntime(error: Error): void {
    for (const pending of this.pendingRequests.values()) {
      clearTimeout(pending.timeout);
      pending.reject(error);
    }
    this.pendingRequests.clear();
    this.settleTurn(error);
    this.terminateProcess();
  }

  private terminateProcess(): void {
    if (this.processExited) return;
    this.child.kill("SIGTERM");
    this.scheduleForceKill();
  }

  private scheduleForceKill(): void {
    if (this.processExited || this.forceKillTimer) return;
    this.forceKillTimer = setTimeout(() => {
      if (!this.processExited) this.child.kill("SIGKILL");
    }, INTERRUPT_GRACE_MS);
    this.forceKillTimer.unref();
  }
}

function approvalPayload(
  method: string,
  requestId: string,
  params: Record<string, unknown>,
): Record<string, unknown> {
  const requestKind = method.includes("commandExecution")
    ? "command"
    : method.includes("fileRead")
      ? "file-read"
      : "file-change";
  const reason = boundedString(params.reason, 2_000);
  const command = boundedString(params.command, 4_000);
  const cwd = boundedString(params.cwd, 2_000);
  const grantRoot = boundedString(params.grantRoot, 2_000);
  const network = asRecord(params.networkApprovalContext);
  return {
    requestId,
    provider: "codex",
    requestKind,
    summary: reason ?? approvalSummary(requestKind),
    ...(readString(params, "itemId") ? { itemId: readString(params, "itemId") } : {}),
    ...(readString(params, "turnId") ? { providerTurnId: readString(params, "turnId") } : {}),
    ...(command ? { command } : {}),
    ...(cwd ? { cwd } : {}),
    ...(grantRoot ? { grantRoot } : {}),
    ...(network
      ? {
          network: {
            ...(boundedString(network.host, 512) ? { host: boundedString(network.host, 512) } : {}),
            ...(boundedString(network.protocol, 64)
              ? { protocol: boundedString(network.protocol, 64) }
              : {}),
          },
        }
      : {}),
  };
}

function userInputPayload(
  requestId: string,
  params: Record<string, unknown>,
): Record<string, unknown> {
  const questions = Array.isArray(params.questions)
    ? params.questions.slice(0, 3).flatMap((value) => {
        const question = asRecord(value);
        const id = boundedString(question?.id, 200);
        const header = boundedString(question?.header, 120);
        const text = boundedString(question?.question, 2_000);
        if (!id || !header || !text) return [];
        const options = Array.isArray(question?.options)
          ? question.options.slice(0, 20).flatMap((optionValue) => {
              const option = asRecord(optionValue);
              const label = boundedString(option?.label, 200);
              const description = boundedString(option?.description, 1_000);
              return label && description ? [{ label, description }] : [];
            })
          : null;
        return [
          {
            id,
            header,
            question: text,
            isOther: question?.isOther === true,
            isSecret: question?.isSecret === true,
            options,
          },
        ];
      })
    : [];
  return {
    requestId,
    provider: "codex",
    questions,
    ...(typeof params.autoResolutionMs === "number"
      ? { autoResolutionMs: Math.max(0, Math.floor(params.autoResolutionMs)) }
      : {}),
  };
}

function codexUserInputAnswers(
  answers: Record<string, unknown>,
): Record<string, { answers: string[] }> {
  return Object.fromEntries(
    Object.entries(answers).map(([questionId, value]) => {
      if (typeof value === "string") return [questionId, { answers: [value] }];
      if (Array.isArray(value)) {
        return [questionId, { answers: value.filter((item): item is string => typeof item === "string") }];
      }
      const record = asRecord(value);
      if (Array.isArray(record?.answers)) {
        return [
          questionId,
          { answers: record.answers.filter((item): item is string => typeof item === "string") },
        ];
      }
      throw new Error(`Codex user-input answer ${questionId} is invalid.`);
    }),
  );
}

function finalAgentText(turn: Record<string, unknown>): string | undefined {
  const items = Array.isArray(turn.items) ? turn.items : [];
  const messages = items.flatMap((value) => {
    const item = asRecord(value);
    return item?.type === "agentMessage" && typeof item.text === "string" ? [item.text] : [];
  });
  return messages.at(-1);
}

function readThreadId(response: Record<string, unknown>): string | undefined {
  return readString(asRecord(response.thread), "id") ?? readString(response, "threadId");
}

function interactionRequestId(id: JsonRpcId): string {
  return `codex:${String(id)}`.slice(0, 200);
}

function approvalSummary(requestKind: string): string {
  if (requestKind === "command") return "Codex requests permission to run a command.";
  if (requestKind === "file-read") return "Codex requests permission to read a file.";
  return "Codex requests permission to change files.";
}

function numericFields(value: Record<string, unknown>): Record<string, number> {
  return Object.fromEntries(
    Object.entries(value).filter((entry): entry is [string, number] => typeof entry[1] === "number"),
  );
}

function boundedString(value: unknown, maximumLength: number): string | undefined {
  return typeof value === "string" && value.trim() ? value.trim().slice(0, maximumLength) : undefined;
}

function trimmedString(value: unknown): string | undefined {
  return boundedString(value, 1_000);
}

function requiredString(value: unknown, label: string): string {
  const result = boundedString(value, 200);
  if (!result) throw new Error(`${label} is required.`);
  return result;
}

function readString(value: Record<string, unknown> | undefined, key: string): string | undefined {
  return value && typeof value[key] === "string" ? value[key] : undefined;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}
