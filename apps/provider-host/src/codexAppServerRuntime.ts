import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createInterface } from "node:readline";

import {
  hasAuthoritativeResumeData,
  type ProviderPrimaryOperation,
  type ProviderReviewTarget,
  type ProviderRunController,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { providerInteractionRequestId } from "./interactionRequestId";
import { ProviderInterruptedError } from "./providerRunErrors";
import {
  classifyProviderResumeFailure,
  providerResumeFallbackWarning,
} from "./providerResumeFallback";
import {
  createTerminalOutputStream,
  terminalCommandSummary,
  terminalCwdLabel,
  terminalResultText,
  type TerminalOutputStream,
  type TerminalRedactor,
} from "./terminalEvents";

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

type CodexTerminalState = {
  terminalId: string;
  commandSummary?: string;
  cwdLabel?: string;
  output: TerminalOutputStream;
  sawOutputDelta: boolean;
};

type CodexRunOptions = {
  input: RunnerInput;
  environment: NodeJS.ProcessEnv;
  redact: TerminalRedactor;
  emit: (message: RunnerMessage) => void;
  authoritativePrompt: string;
  interactive: boolean;
  operation?: ProviderPrimaryOperation;
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
  private readonly commandTerminals = new Map<string, CodexTerminalState>();
  private readonly outputText: string[] = [];
  private readonly turnCompletion: Promise<Record<string, unknown>>;
  private resolveTurn!: (turn: Record<string, unknown>) => void;
  private rejectTurn!: (error: Error) => void;
  private nextRequestId = 1;
  private terminalSequence = 0;
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

      const operation = this.options.operation?.commandType;
      const resumed = await this.openThread(
        operation === "CompactSession",
        operation === "StartReview",
      );
      if (this.options.operation?.commandType === "CompactSession") {
        return await this.runCompact();
      }
      if (this.options.operation?.commandType === "StartReview") {
        return await this.runReview(this.options.operation.payload.target);
      }
      const prompt = resumed
        ? this.options.input.workload.inputText
        : this.options.authoritativePrompt;
      const turnParams = {
        threadId: this.threadId,
        input: [{ type: "text", text: prompt, text_elements: [] }],
        ...(trimmedString(this.options.input.workload.model)
          ? { model: trimmedString(this.options.input.workload.model) }
          : {}),
        ...(this.options.interactive ? { collaborationMode: this.collaborationMode() } : {}),
      };
      const response = asRecord(
        await this.sendRequest("turn/start", {
          ...turnParams,
        }),
      );
      this.turnId = readString(asRecord(response?.turn), "id");
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

  private async runCompact(): Promise<Extract<RunnerMessage, { type: "result" }>> {
    if (!this.threadId) {
      throw new Error("Codex app-server native compact requires a resumed Provider Thread.");
    }
    await this.sendRequest("thread/compact/start", { threadId: this.threadId });
    if (this.interruptRequested) this.interrupt();
    const completed = await this.turnCompletion;
    const boundary = codexCompactionBoundary(completed);
    return {
      type: "result",
      output: {
        provider: "codex",
        operation: "compact",
        supportMode: "native",
        boundary,
      },
      providerResumeCursor: this.threadId,
    };
  }

  private async runReview(
    target: ProviderReviewTarget,
  ): Promise<Extract<RunnerMessage, { type: "result" }>> {
    if (!this.threadId) {
      throw new Error("Codex app-server native review requires a resumed Provider Thread.");
    }
    const response = asRecord(
      await this.sendRequest("review/start", {
        threadId: this.threadId,
        delivery: "inline",
        target: codexReviewTarget(target),
      }),
    );
    this.turnId = readString(asRecord(response?.turn), "id");
    if (!this.turnId) {
      throw new Error("Codex app-server review/start response did not include a turn id.");
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
        operation: "review",
        supportMode: "native",
        providerTurnId: this.turnId,
        text: this.outputText.join(""),
      },
      providerResumeCursor: this.threadId,
    };
  }

  private async openThread(
    requireNativeResume = false,
    allowFreshThreadOnResumeFailure = false,
  ): Promise<boolean> {
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
        this.model =
          readString(response, "model") ?? trimmedString(this.options.input.workload.model);
        if (!this.threadId) {
          throw new Error("Codex app-server thread/resume response did not include a thread id.");
        }
        return true;
      } catch (error) {
        if (requireNativeResume) {
          const detail = error instanceof Error ? error.message : String(error);
          throw new Error(`Codex app-server session resume is invalid: ${detail}`);
        }
        const reasonCode = classifyProviderResumeFailure(error);
        if (!reasonCode) throw error;
        if (!historyAvailable && !allowFreshThreadOnResumeFailure) {
          const detail = error instanceof Error ? error.message : String(error);
          throw new Error(`Codex app-server session resume is invalid: ${detail}`);
        }
        if (historyAvailable) {
          this.options.emit(providerResumeFallbackWarning(this.options.input, "codex", reasonCode));
        }
      }
    }

    if (requireNativeResume) {
      throw new Error("Codex app-server native Session operation requires a Provider Cursor.");
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
            `Codex app-server exited before operation completion (code=${code ?? "null"}, signal=${signal ?? "null"}).`,
        ),
      );
    });
  }

  private collaborationMode(): Record<string, unknown> {
    const model = this.model ?? trimmedString(this.options.input.workload.model);
    if (!model) {
      throw new Error("Codex app-server did not report a model required for collaboration mode.");
    }
    return {
      mode: this.options.input.workload.interactionMode ?? "default",
      settings: {
        model,
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
    const requestId = interactionRequestId(request.id, this.options.input.execution.generation);
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
      case "item/commandExecution/outputDelta": {
        const itemId = readString(params, "itemId");
        const delta = typeof params.delta === "string" ? params.delta : undefined;
        if (!itemId || delta === undefined) {
          this.failRuntime(
            new Error("Codex app-server command output delta omitted itemId or delta."),
          );
          return;
        }
        if (
          (this.threadId && readString(params, "threadId") !== this.threadId) ||
          (this.turnId && readString(params, "turnId") !== this.turnId)
        ) {
          return;
        }
        const terminal = this.commandTerminalState(undefined, itemId);
        terminal.sawOutputDelta = true;
        terminal.output.write(delta);
        return;
      }
      case "item/started":
      case "item/completed": {
        const item = asRecord(params.item);
        const itemType = readString(item, "type");
        if (!itemType || itemType === "agentMessage" || itemType === "userMessage") return;
        const itemId = readString(item, "id");
        const isCommand = isCommandExecutionItem(itemType);
        const terminal = isCommand ? this.commandTerminalState(item, itemId) : undefined;
        const exitCode = readSafeInteger(item, "exitCode");
        const signal = readString(item, "signal");
        const itemStatus = readString(item, "status")?.toLowerCase();
        const declined = notification.method === "item/completed" && itemStatus === "declined";
        const failed =
          notification.method === "item/completed" &&
          ((exitCode !== undefined && exitCode !== 0) ||
            itemStatus === "failed" ||
            itemStatus === "error");
        if (terminal && notification.method === "item/completed") {
          if (!terminal.sawOutputDelta) terminal.output.write(codexTerminalOutput(item));
          terminal.output.flush();
        }
        const terminalBytes = terminal?.output.bytesWritten() ?? 0;
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: {
            provider: "codex",
            itemType,
            status:
              notification.method === "item/started"
                ? "started"
                : declined
                  ? "declined"
                  : failed
                    ? "failed"
                    : "completed",
            ...(itemId ? { itemId } : {}),
            ...(terminal
              ? {
                  terminalId: terminal.terminalId,
                  terminalEventType:
                    notification.method === "item/started"
                      ? "terminal.started"
                      : failed || declined
                        ? "terminal.failed"
                        : "terminal.exited",
                  ...(terminal.commandSummary ? { commandSummary: terminal.commandSummary } : {}),
                  ...(terminal.cwdLabel ? { cwdLabel: terminal.cwdLabel } : {}),
                  ...(exitCode !== undefined ? { exitCode } : {}),
                  ...(signal ? { signal } : {}),
                  ...(notification.method === "item/completed"
                    ? {
                        totalBytes: terminalBytes,
                        previewBytes: terminalBytes,
                        segmentCount: 0,
                        truncated: false,
                      }
                    : {}),
                  ...(failed || declined
                    ? {
                        failureKind: signal
                          ? "signal"
                          : exitCode !== undefined && exitCode !== 0
                            ? "exit"
                            : "provider_error",
                      }
                    : {}),
                }
              : {}),
          },
        });
        if (terminal && notification.method === "item/completed" && itemId) {
          this.commandTerminals.delete(itemId);
        }
        if (
          notification.method === "item/completed" &&
          this.options.operation?.commandType === "StartReview" &&
          itemType === "exitedReviewMode" &&
          this.outputText.length === 0
        ) {
          const review = readString(item, "review");
          if (review) {
            const text = this.options.redact(review);
            this.outputText.push(text);
            this.options.emit({
              type: "event",
              eventType: "runtime.output.delta",
              payload: { text },
            });
          }
        }
        if (
          notification.method === "item/completed" &&
          this.options.operation?.commandType === "CompactSession" &&
          itemType === "contextCompaction"
        ) {
          this.settleTurn(undefined, {
            terminalKind: "contextCompaction",
            item: item ?? {},
            ...(readString(params, "turnId") ? { turnId: readString(params, "turnId") } : {}),
          });
        }
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
          const requestId = interactionRequestId(
            resolvedId,
            this.options.input.execution.generation,
          );
          this.pendingApprovals.delete(requestId);
          this.pendingUserInputs.delete(requestId);
        }
        return;
      }
      case "turn/aborted": {
        this.failOpenCommandTerminals();
        this.settleTurn(new ProviderInterruptedError());
        return;
      }
      case "turn/completed": {
        if (this.options.operation?.commandType === "CompactSession") {
          // The RPC acknowledgement and a generic Turn terminal do not prove
          // that compaction committed. Only contextCompaction or the legacy
          // thread/compacted notification may settle the primary operation.
          return;
        }
        const turn = asRecord(params.turn);
        const completedTurnId = readString(turn, "id");
        if (this.turnId && completedTurnId && completedTurnId !== this.turnId) return;
        const status = readString(turn, "status");
        if (status === "completed") {
          if (this.commandTerminals.size > 0) {
            this.failOpenCommandTerminals();
            this.settleTurn(
              new Error("Codex app-server completed a Turn with an open command execution."),
            );
            return;
          }
          this.settleTurn(undefined, turn ?? {});
        } else if (status === "interrupted") {
          this.failOpenCommandTerminals();
          this.settleTurn(new ProviderInterruptedError());
        } else {
          this.failOpenCommandTerminals();
          const message =
            readString(asRecord(turn?.error), "message") ??
            this.lastError ??
            "Codex app-server turn failed.";
          this.settleTurn(new Error(this.options.redact(message)));
        }
        return;
      }
      case "thread/compacted": {
        if (this.options.operation?.commandType !== "CompactSession") return;
        const notificationThreadId = readString(params, "threadId");
        if (this.threadId && notificationThreadId && notificationThreadId !== this.threadId) {
          return;
        }
        this.settleTurn(undefined, {
          terminalKind: "thread/compacted",
          ...(readString(params, "turnId") ? { turnId: readString(params, "turnId") } : {}),
        });
        return;
      }
      default:
        return;
    }
  }

  private commandTerminalState(
    item: Record<string, unknown> | undefined,
    itemId: string | undefined,
  ): CodexTerminalState {
    if (itemId) {
      const existing = this.commandTerminals.get(itemId);
      if (existing) {
        if (!existing.commandSummary) {
          const commandSummary = terminalCommandSummary(item?.command, this.options.redact);
          if (commandSummary) existing.commandSummary = commandSummary;
        }
        if (!existing.cwdLabel) {
          const cwdLabel = terminalCwdLabel(this.options.input.workspaceDirectory, item?.cwd);
          if (cwdLabel) existing.cwdLabel = cwdLabel;
        }
        return existing;
      }
    }
    const commandSummary = terminalCommandSummary(item?.command, this.options.redact);
    const cwdLabel = terminalCwdLabel(this.options.input.workspaceDirectory, item?.cwd);
    const terminalId = itemId ?? `codex-terminal-${++this.terminalSequence}`;
    const terminal: CodexTerminalState = {
      terminalId,
      ...(commandSummary ? { commandSummary } : {}),
      ...(cwdLabel ? { cwdLabel } : {}),
      output: createTerminalOutputStream({
        emit: this.options.emit,
        provider: "codex",
        terminalId,
        redact: this.options.redact,
      }),
      sawOutputDelta: false,
    };
    if (itemId) this.commandTerminals.set(itemId, terminal);
    return terminal;
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
    this.writeMessage({
      id: pending.jsonRpcId,
      result: { answers: codexUserInputAnswers(answers) },
    });
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

  private sendRequest(
    method: string,
    params: unknown,
    timeoutMs = REQUEST_TIMEOUT_MS,
  ): Promise<unknown> {
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
    this.failOpenCommandTerminals();
    this.settleTurn(error);
    this.terminateProcess();
  }

  private failOpenCommandTerminals(): void {
    for (const [itemId, terminal] of this.commandTerminals) {
      terminal.output.flush();
      const terminalBytes = terminal.output.bytesWritten();
      this.options.emit({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: {
          provider: "codex",
          itemType: "commandExecution",
          itemId,
          status: "failed",
          terminalId: terminal.terminalId,
          terminalEventType: "terminal.failed",
          ...(terminal.commandSummary ? { commandSummary: terminal.commandSummary } : {}),
          ...(terminal.cwdLabel ? { cwdLabel: terminal.cwdLabel } : {}),
          failureKind: "provider_error",
          totalBytes: terminalBytes,
          previewBytes: terminalBytes,
          segmentCount: 0,
          truncated: false,
        },
      });
    }
    this.commandTerminals.clear();
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
        return [
          questionId,
          { answers: value.filter((item): item is string => typeof item === "string") },
        ];
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

function codexReviewTarget(target: ProviderReviewTarget): Record<string, unknown> {
  if (target.type === "uncommittedChanges") return { type: target.type };
  const branch = boundedString(target.branch, 500);
  if (!branch || /[\r\n\0]/u.test(branch)) {
    throw new Error("StartReview baseBranch target requires a valid branch.");
  }
  return { type: target.type, branch };
}

function codexCompactionBoundary(completed: Record<string, unknown>): Record<string, unknown> {
  const item = asRecord(completed.item);
  const summary = boundedString(item?.summary, 20_000) ?? boundedString(completed.summary, 20_000);
  const providerItemId = readString(item, "id");
  const providerTurnId = readString(completed, "turnId");
  const terminalKind = readString(completed, "terminalKind") ?? "contextCompaction";
  return {
    kind: "context_compaction",
    terminalKind,
    summaryAvailable: summary !== undefined,
    ...(providerItemId ? { providerItemId } : {}),
    ...(providerTurnId ? { providerTurnId } : {}),
    ...(summary
      ? { summary }
      : {
          detail:
            "Codex completed native context compaction without exposing a summary on the Provider wire.",
        }),
  };
}

function readThreadId(response: Record<string, unknown> | undefined): string | undefined {
  return readString(asRecord(response?.thread), "id") ?? readString(response, "threadId");
}

function interactionRequestId(id: JsonRpcId, generation?: number): string {
  return providerInteractionRequestId("codex", generation, undefined, id);
}

function approvalSummary(requestKind: string): string {
  if (requestKind === "command") return "Codex requests permission to run a command.";
  if (requestKind === "file-read") return "Codex requests permission to read a file.";
  return "Codex requests permission to change files.";
}

function numericFields(value: Record<string, unknown>): Record<string, number> {
  return Object.fromEntries(
    Object.entries(value).filter(
      (entry): entry is [string, number] => typeof entry[1] === "number",
    ),
  );
}

function isCommandExecutionItem(itemType: string): boolean {
  const normalized = itemType.replaceAll(/[^a-z0-9]/giu, "").toLowerCase();
  return normalized.includes("command") || normalized === "bash" || normalized === "shell";
}

function codexTerminalOutput(item: Record<string, unknown> | undefined): string {
  if (!item) return "";
  const primary =
    terminalResultText(item.aggregatedOutput) ||
    terminalResultText(item.output) ||
    terminalResultText(item.result);
  if (primary) return primary;
  return [terminalResultText(item.stdout), terminalResultText(item.stderr)]
    .filter((value) => value.length > 0)
    .join("\n");
}

function readSafeInteger(
  value: Record<string, unknown> | undefined,
  key: string,
): number | undefined {
  const candidate = value?.[key];
  return typeof candidate === "number" && Number.isSafeInteger(candidate) ? candidate : undefined;
}

function boundedString(value: unknown, maximumLength: number): string | undefined {
  return typeof value === "string" && value.trim()
    ? value.trim().slice(0, maximumLength)
    : undefined;
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
