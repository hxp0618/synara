import {
  query,
  type CanUseTool,
  type HookCallback,
  type Options as ClaudeQueryOptions,
  type PermissionResult,
  type SDKMessage,
  type SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";

import {
  hasAuthoritativeResumeData,
  type ProviderRunController,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { providerInteractionRequestId } from "./interactionRequestId";
import { ProviderInterruptedError } from "./providerRunErrors";

export type ClaudeQueryRuntime = AsyncIterable<SDKMessage> & {
  interrupt: () => Promise<unknown>;
  close: () => void;
};

export type ClaudeQueryFactory = (input: {
  prompt: string | AsyncIterable<SDKUserMessage>;
  options?: ClaudeQueryOptions;
}) => ClaudeQueryRuntime;

type ClaudeRunOptions = {
  input: RunnerInput;
  environment: NodeJS.ProcessEnv;
  redact: (value: string) => string;
  emit: (message: RunnerMessage) => void;
  authoritativePrompt: string;
  interactive: boolean;
  queryFactory?: ClaudeQueryFactory;
};

type AttemptState = {
  cursor?: string;
  model?: string;
  outputText: string[];
  sawPartialText: boolean;
  hadTurnActivity: boolean;
  tools: Map<string, string>;
};

type PendingApproval = {
  input: Record<string, unknown>;
  cleanup: () => void;
  resolve: (result: PermissionResult) => void;
};

type UserInputQuestion = {
  id: string;
  header: string;
  question: string;
  options: Array<{ label: string; description: string }>;
  multiSelect: boolean;
};

type PendingUserInput = {
  input: Record<string, unknown>;
  questions: UserInputQuestion[];
  cleanup: () => void;
  resolve: (result: PermissionResult) => void;
};

type PromptStream = {
  stream: AsyncIterable<SDKUserMessage>;
  push: (text: string) => void;
  close: () => void;
};

const INTERRUPT_GRACE_MS = 2_000;
const MAX_ERROR_BYTES = 64 * 1024;
const CLAUDE_SYSTEM_PROMPT_APPEND = [
  "You are running inside Synara, a coding app that embeds the Claude Agent SDK.",
  "Treat the current working directory as the active workspace for the task.",
  "When asked about the project, inspect the workspace before asking where to look.",
].join("\n");

const defaultQueryFactory: ClaudeQueryFactory = (input) => query(input);

export function startClaudeAgentSdkRun(options: ClaudeRunOptions): ProviderRunController {
  const runtime = new ClaudeAgentSdkRuntime(options);
  return runtime.start();
}

class ClaudeAgentSdkRuntime {
  private readonly queryFactory: ClaudeQueryFactory;
  private readonly pendingApprovals = new Map<string, PendingApproval>();
  private readonly pendingUserInputs = new Map<string, PendingUserInput>();
  private activeQuery: ClaudeQueryRuntime | undefined;
  private activePromptStream: PromptStream | undefined;
  private resumeCursor: string | undefined;
  private forceCloseTimer: NodeJS.Timeout | undefined;
  private requestSequence = 0;
  private interruptRequested = false;
  private settled = false;

  constructor(private readonly options: ClaudeRunOptions) {
    this.queryFactory = options.queryFactory ?? defaultQueryFactory;
  }

  start(): ProviderRunController {
    const result = this.run().finally(() => {
      this.settled = true;
      if (this.forceCloseTimer) clearTimeout(this.forceCloseTimer);
      this.cancelPendingInteractions();
    });
    return {
      result,
      interrupt: () => this.interrupt(),
      getResumeCursor: () => this.resumeCursor,
      steer: (payload) => this.steer(payload),
      resolveApproval: (payload) => this.resolveApproval(payload),
      resolveUserInput: (payload) => this.resolveUserInput(payload),
    };
  }

  private async run(): Promise<Extract<RunnerMessage, { type: "result" }>> {
    const cursor = trimmedString(this.options.input.providerResumeCursor);
    const historyAvailable = hasAuthoritativeResumeData(this.options.input.workload);
    if (cursor) {
      try {
        return await this.runAttempt(this.options.input.workload.inputText, cursor);
      } catch (error) {
        if (error instanceof ProviderInterruptedError) throw error;
        if (
          !historyAvailable ||
          !(error instanceof ClaudeAttemptError) ||
          error.hadTurnActivity ||
          !isResumeFailure(error.message)
        ) {
          throw error;
        }
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.warning",
          payload: {
            provider: "claudeAgent",
            message:
              "Native Claude resume was unavailable; rebuilt the turn from authoritative history.",
          },
        });
      }
    }
    return this.runAttempt(this.options.authoritativePrompt);
  }

  private async runAttempt(
    prompt: string,
    resume?: string,
  ): Promise<Extract<RunnerMessage, { type: "result" }>> {
    const state: AttemptState = {
      outputText: [],
      sawPartialText: false,
      hadTurnActivity: false,
      tools: new Map(),
    };
    const promptStream = createPromptStream(prompt);
    let runtime: ClaudeQueryRuntime | undefined;
    try {
      runtime = this.queryFactory({
        prompt: promptStream.stream,
        options: this.queryOptions(state, resume),
      });
      this.activeQuery = runtime;
      this.activePromptStream = promptStream;
      if (this.interruptRequested) this.requestNativeInterrupt(runtime);

      for await (const message of runtime) {
        const terminal = this.handleMessage(message, state);
        if (terminal) return terminal;
      }
      if (this.interruptRequested) throw new ProviderInterruptedError();
      throw new Error("Claude Agent SDK ended before emitting a terminal result.");
    } catch (error) {
      if (this.interruptRequested || error instanceof ProviderInterruptedError) {
        throw new ProviderInterruptedError();
      }
      throw new ClaudeAttemptError(this.safeErrorMessage(error), state.hadTurnActivity);
    } finally {
      promptStream.close();
      if (this.activeQuery === runtime) this.activeQuery = undefined;
      if (this.activePromptStream === promptStream) this.activePromptStream = undefined;
      if (this.forceCloseTimer) {
        clearTimeout(this.forceCloseTimer);
        this.forceCloseTimer = undefined;
      }
      try {
        runtime?.close();
      } catch {
        // The terminal result or surfaced runtime error remains authoritative.
      }
    }
  }

  private queryOptions(state: AttemptState, resume?: string): ClaudeQueryOptions {
    const permissionMode = this.permissionMode();
    return {
      cwd: this.options.input.workspaceDirectory,
      ...(trimmedString(this.options.input.workload.model)
        ? { model: trimmedString(this.options.input.workload.model) }
        : {}),
      pathToClaudeCodeExecutable: "claude",
      settingSources: ["user", "project", "local"],
      systemPrompt: {
        type: "preset",
        preset: "claude_code",
        append: CLAUDE_SYSTEM_PROMPT_APPEND,
      },
      permissionMode,
      ...(permissionMode === "bypassPermissions"
        ? { allowDangerouslySkipPermissions: true }
        : {}),
      ...(resume ? { resume } : {}),
      includePartialMessages: true,
      hooks: {
        PreToolUse: [{ hooks: [this.createPreToolUseHook(state)] }],
      },
      ...(this.options.interactive ? { canUseTool: this.createCanUseTool(state) } : {}),
      env: {
        ...this.options.environment,
        CLAUDE_AGENT_SDK_CLIENT_APP: "synara-provider-host/0.2.0",
      },
    };
  }

  private permissionMode(): "default" | "bypassPermissions" | "plan" {
    if (!this.options.interactive) return "bypassPermissions";
    if (this.options.input.workload.interactionMode === "plan") return "plan";
    // Keep the SDK callback active in full-access mode so AskUserQuestion can
    // still round-trip through Synara. The callback auto-allows ordinary tool
    // requests, while approval-required mode waits for a durable resolution.
    return "default";
  }

  private createPreToolUseHook(state: AttemptState): HookCallback {
    return async (input) => {
      if (input.hook_event_name !== "PreToolUse") return { continue: true };
      const toolName = input.tool_name;
      const toolInput = asRecord(input.tool_input) ?? {};
      state.hadTurnActivity = true;
      const decision = this.preToolPermissionDecision(toolName);
      if (!decision) return { continue: true };
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: decision,
          permissionDecisionReason:
            decision === "ask"
              ? "Synara requires a durable interaction decision for this tool."
              : "Synara runtime mode allows this tool for the current Turn.",
          ...(decision === "allow" ? { updatedInput: toolInput } : {}),
        },
      };
    };
  }

  private preToolPermissionDecision(toolName: string): "allow" | "ask" | undefined {
    if (!this.options.interactive) return undefined;
    if (toolName === "AskUserQuestion" || toolName === "ExitPlanMode") return "ask";
    if (this.options.input.workload.interactionMode === "plan") return undefined;
    if (this.options.input.workload.runtimeMode !== "approval-required") return "allow";
    return isReadOnlyTool(toolName) ? "allow" : "ask";
  }

  private createCanUseTool(state: AttemptState): CanUseTool {
    return async (toolName, toolInput, callbackOptions) => {
      state.hadTurnActivity = true;
      if (toolName === "AskUserQuestion") {
        if (!this.options.interactive) {
          return { behavior: "deny", message: "Interactive user input is unavailable." };
        }
        return this.requestUserInput(toolInput, callbackOptions);
      }
      if (
        toolName === "ExitPlanMode" &&
        this.options.input.workload.interactionMode === "plan"
      ) {
        this.options.emit({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: { provider: "claudeAgent", itemType: "plan", status: "updated" },
        });
        return {
          behavior: "deny",
          message:
            "Synara captured the proposed plan. Stop and wait for a later implementation turn.",
        };
      }
      if (
        !this.options.interactive ||
        this.options.input.workload.runtimeMode !== "approval-required"
      ) {
        return { behavior: "allow", updatedInput: toolInput };
      }
      return this.requestApproval(toolName, toolInput, callbackOptions);
    };
  }

  private requestApproval(
    toolName: string,
    toolInput: Record<string, unknown>,
    callbackOptions: Parameters<CanUseTool>[2],
  ): Promise<PermissionResult> {
    if (callbackOptions.signal.aborted) {
      return Promise.resolve({ behavior: "deny", message: "Tool execution was cancelled." });
    }
    const requestId = this.uniqueRequestId("approval", callbackOptions.toolUseID);
    return new Promise((resolve) => {
      const pending: PendingApproval = {
        input: toolInput,
        cleanup: () => {},
        resolve,
      };
      this.pendingApprovals.set(requestId, pending);
      pending.cleanup = registerAbort(callbackOptions.signal, () => {
        this.pendingApprovals.delete(requestId);
        resolve({ behavior: "deny", message: "Tool execution was cancelled." });
      });
      if (!this.pendingApprovals.has(requestId)) return;
      this.options.emit({
        type: "interaction",
        interactionType: "approval",
        payload: this.approvalPayload(requestId, toolName, toolInput, callbackOptions),
      });
    });
  }

  private requestUserInput(
    toolInput: Record<string, unknown>,
    callbackOptions: Parameters<CanUseTool>[2],
  ): Promise<PermissionResult> {
    if (callbackOptions.signal.aborted) {
      return Promise.resolve({ behavior: "deny", message: "User input was cancelled." });
    }
    const questions = this.userInputQuestions(toolInput);
    if (questions.length === 0) {
      return Promise.resolve({
        behavior: "deny",
        message: "Claude requested user input without any valid questions.",
      });
    }
    const requestId = this.uniqueRequestId("user-input", callbackOptions.toolUseID);
    return new Promise((resolve) => {
      const pending: PendingUserInput = {
        input: toolInput,
        questions,
        cleanup: () => {},
        resolve,
      };
      this.pendingUserInputs.set(requestId, pending);
      pending.cleanup = registerAbort(callbackOptions.signal, () => {
        this.pendingUserInputs.delete(requestId);
        resolve({ behavior: "deny", message: "User input was cancelled." });
      });
      if (!this.pendingUserInputs.has(requestId)) return;
      this.options.emit({
        type: "interaction",
        interactionType: "user-input",
        payload: {
          requestId,
          provider: "claudeAgent",
          ...(callbackOptions.toolUseID ? { toolUseId: callbackOptions.toolUseID } : {}),
          questions,
        },
      });
    });
  }

  private resolveApproval(payload: Record<string, unknown>): void {
    const requestId = requiredString(payload.requestId, "ResolveApproval requestId");
    const pending = this.pendingApprovals.get(requestId);
    if (!pending) throw new Error(`Unknown pending Claude approval request: ${requestId}`);
    const resolution = asRecord(payload.resolution);
    const decision = readString(resolution, "decision");
    if (decision !== "accept" && decision !== "decline") {
      throw new Error("Claude approval resolution must be accept or decline.");
    }
    this.pendingApprovals.delete(requestId);
    pending.cleanup();
    pending.resolve(
      decision === "accept"
        ? { behavior: "allow", updatedInput: pending.input }
        : { behavior: "deny", message: "User declined tool execution." },
    );
  }

  private resolveUserInput(payload: Record<string, unknown>): void {
    const requestId = requiredString(payload.requestId, "ResolveUserInput requestId");
    const pending = this.pendingUserInputs.get(requestId);
    if (!pending) throw new Error(`Unknown pending Claude user-input request: ${requestId}`);
    const resolution = asRecord(payload.resolution);
    const answers = asRecord(resolution?.answers);
    if (!answers) throw new Error("Claude user-input resolution must include an answers object.");
    this.pendingUserInputs.delete(requestId);
    pending.cleanup();
    pending.resolve({
      behavior: "allow",
      updatedInput: {
        questions: pending.input.questions,
        answers: remapAnswers(pending.questions, answers),
      },
    });
  }

  private steer(payload: Record<string, unknown>): void {
    const inputText = requiredString(payload.inputText, "SteerTurn inputText");
    if (this.settled || this.interruptRequested || !this.activePromptStream) {
      throw new Error("Claude Agent SDK does not have an active steerable Query.");
    }
    this.activePromptStream.push(inputText);
  }

  private interrupt(): void {
    if (this.settled || this.interruptRequested) return;
    this.interruptRequested = true;
    this.cancelPendingInteractions();
    if (this.activeQuery) this.requestNativeInterrupt(this.activeQuery);
  }

  private requestNativeInterrupt(runtime: ClaudeQueryRuntime): void {
    void runtime.interrupt().catch(() => {
      try {
        runtime.close();
      } catch {
        // The interrupted terminal state is reported through the result promise.
      }
    });
    if (this.forceCloseTimer) return;
    this.forceCloseTimer = setTimeout(() => {
      try {
        runtime.close();
      } catch {
        // Best-effort cleanup after the native interrupt grace period.
      }
    }, INTERRUPT_GRACE_MS);
    this.forceCloseTimer.unref();
  }

  private handleMessage(
    message: SDKMessage,
    state: AttemptState,
  ): Extract<RunnerMessage, { type: "result" }> | undefined {
    const record = asRecord(message);
    if (!record) return undefined;
    const sessionId = readString(record, "session_id");
    if (sessionId) {
      state.cursor = sessionId;
      this.resumeCursor = sessionId;
    }
    const type = readString(record, "type");
    if (type === "system" && readString(record, "subtype") === "init") {
      state.model = readString(record, "model") ?? state.model;
      return undefined;
    }
    if (type === "stream_event") {
      this.handleStreamEvent(record, state);
      return undefined;
    }
    if (type === "assistant") {
      this.handleAssistant(record, state);
      return undefined;
    }
    if (type === "user") {
      this.handleUserMessage(record, state);
      return undefined;
    }
    if (type === "tool_progress") {
      state.hadTurnActivity = true;
      const toolName = readString(record, "tool_name") ?? "tool";
      const toolUseId = readString(record, "tool_use_id");
      if (!isClientSurfacedTool(toolName)) {
        this.emitToolActivity(toolName, "updated", toolUseId);
      }
      return undefined;
    }
    if (type !== "result") return undefined;

    const usage = asRecord(record.usage);
    if (usage) {
      this.options.emit({
        type: "event",
        eventType: "runtime.usage",
        payload: { provider: "claudeAgent", ...numericFields(usage) },
      });
    }
    this.completeOpenTools(state);
    if (readString(record, "subtype") !== "success" || record.is_error === true) {
      throw new Error(resultErrorMessage(record));
    }
    if (state.outputText.length === 0) {
      const resultText = readString(record, "result");
      if (resultText) this.appendOutput(resultText, state);
    }
    return {
      type: "result",
      output: {
        provider: "claudeAgent",
        model: state.model ?? this.options.input.workload.model ?? null,
        text: state.outputText.join(""),
      },
      ...(state.cursor ? { providerResumeCursor: state.cursor } : {}),
    };
  }

  private handleStreamEvent(message: Record<string, unknown>, state: AttemptState): void {
    const event = asRecord(message.event);
    if (readString(event, "type") !== "content_block_delta") return;
    const delta = asRecord(event?.delta);
    if (readString(delta, "type") !== "text_delta") return;
    const text = readString(delta, "text");
    if (!text) return;
    state.hadTurnActivity = true;
    state.sawPartialText = true;
    this.appendOutput(text, state);
  }

  private handleAssistant(message: Record<string, unknown>, state: AttemptState): void {
    const content = asRecord(message.message)?.content;
    if (!Array.isArray(content)) return;
    state.hadTurnActivity = true;
    for (const value of content) {
      const block = asRecord(value);
      const blockType = readString(block, "type");
      if (blockType === "text" && !state.sawPartialText) {
        const text = readString(block, "text");
        if (text) this.appendOutput(text, state);
        continue;
      }
      if (blockType !== "tool_use") continue;
      const toolName = readString(block, "name") ?? "tool";
      const toolUseId = readString(block, "id") ?? this.uniqueRequestId("tool");
      state.tools.set(toolUseId, toolName);
      if (!isClientSurfacedTool(toolName)) {
        this.emitToolActivity(toolName, "started", toolUseId);
      }
    }
  }

  private handleUserMessage(message: Record<string, unknown>, state: AttemptState): void {
    const content = asRecord(message.message)?.content;
    if (!Array.isArray(content)) return;
    for (const value of content) {
      const block = asRecord(value);
      if (readString(block, "type") !== "tool_result") continue;
      const toolUseId = readString(block, "tool_use_id");
      if (!toolUseId) continue;
      const toolName = state.tools.get(toolUseId);
      if (!toolName) continue;
      state.tools.delete(toolUseId);
      if (!isClientSurfacedTool(toolName)) {
        this.emitToolActivity(toolName, block?.is_error === true ? "failed" : "completed", toolUseId);
      }
    }
  }

  private appendOutput(value: string, state: AttemptState): void {
    const text = this.options.redact(value);
    if (!text) return;
    state.outputText.push(text);
    this.options.emit({
      type: "event",
      eventType: "runtime.output.delta",
      payload: { text },
    });
  }

  private completeOpenTools(state: AttemptState): void {
    for (const [toolUseId, toolName] of state.tools) {
      if (!isClientSurfacedTool(toolName)) {
        this.emitToolActivity(toolName, "completed", toolUseId);
      }
    }
    state.tools.clear();
  }

  private emitToolActivity(toolName: string, status: string, toolUseId?: string): void {
    this.options.emit({
      type: "event",
      eventType: "runtime.provider.activity",
      payload: {
        provider: "claudeAgent",
        itemType: this.safeString(toolName, 200) ?? "tool",
        status,
        ...(toolUseId ? { itemId: this.safeString(toolUseId, 200) } : {}),
      },
    });
  }

  private approvalPayload(
    requestId: string,
    toolName: string,
    toolInput: Record<string, unknown>,
    callbackOptions: Parameters<CanUseTool>[2],
  ): Record<string, unknown> {
    const requestKind = classifyRequestKind(toolName);
    const command = this.safeString(toolInput.command ?? toolInput.cmd, 4_000);
    const path = this.safeString(
      toolInput.file_path ?? toolInput.path ?? callbackOptions.blockedPath,
      2_000,
    );
    const summary =
      this.safeString(callbackOptions.title, 2_000) ??
      this.safeString(callbackOptions.description, 2_000) ??
      this.safeString(callbackOptions.decisionReason, 2_000) ??
      approvalSummary(requestKind, toolName);
    return {
      requestId,
      provider: "claudeAgent",
      requestKind,
      summary,
      toolName: this.safeString(toolName, 200) ?? "tool",
      ...(callbackOptions.toolUseID ? { toolUseId: callbackOptions.toolUseID } : {}),
      ...(command ? { command } : {}),
      ...(path ? { path } : {}),
      ...(requestKind === "command"
        ? { cwd: this.options.input.workspaceDirectory }
        : {}),
    };
  }

  private userInputQuestions(toolInput: Record<string, unknown>): UserInputQuestion[] {
    if (!Array.isArray(toolInput.questions)) return [];
    return toolInput.questions.slice(0, 3).flatMap((value, index) => {
      const question = asRecord(value);
      const text = this.safeString(question?.question, 2_000);
      if (!text) return [];
      const header = this.safeString(question?.header, 120) ?? `Question ${index + 1}`;
      const options = Array.isArray(question?.options)
        ? question.options.slice(0, 20).flatMap((optionValue) => {
            const option = asRecord(optionValue);
            const label = this.safeString(option?.label, 200);
            if (!label) return [];
            return [
              {
                label,
                description: this.safeString(option?.description, 1_000) ?? "",
              },
            ];
          })
        : [];
      return [
        {
          id: `question-${index + 1}`,
          header,
          question: text,
          options,
          multiSelect: question?.multiSelect === true,
        },
      ];
    });
  }

  private uniqueRequestId(kind: string, nativeId?: string): string {
    const base = this.safeString(nativeId, 200) ?? String(++this.requestSequence);
    const generation = this.options.input.execution.generation;
    let requestId = providerInteractionRequestId("claude", generation, kind, base);
    while (this.pendingApprovals.has(requestId) || this.pendingUserInputs.has(requestId)) {
      requestId = providerInteractionRequestId(
        "claude",
        generation,
        kind,
        base,
        ++this.requestSequence,
      );
    }
    return requestId;
  }

  private cancelPendingInteractions(): void {
    for (const pending of this.pendingApprovals.values()) {
      pending.cleanup();
      pending.resolve({ behavior: "deny", message: "Tool execution was cancelled." });
    }
    this.pendingApprovals.clear();
    for (const pending of this.pendingUserInputs.values()) {
      pending.cleanup();
      pending.resolve({ behavior: "deny", message: "User input was cancelled." });
    }
    this.pendingUserInputs.clear();
  }

  private safeString(value: unknown, maximumLength: number): string | undefined {
    const text = boundedString(value, maximumLength);
    return text ? this.options.redact(text) : undefined;
  }

  private safeErrorMessage(error: unknown): string {
    const raw = error instanceof Error ? error.message : String(error);
    const redacted = this.options.redact(raw || "Claude Agent SDK runtime failed.");
    return Buffer.from(redacted).subarray(0, MAX_ERROR_BYTES).toString("utf8");
  }
}

class ClaudeAttemptError extends Error {
  constructor(
    message: string,
    readonly hadTurnActivity: boolean,
  ) {
    super(message);
    this.name = "ClaudeAttemptError";
  }
}

function createPromptStream(prompt: string): PromptStream {
  const queue: SDKUserMessage[] = [claudeUserMessage(prompt)];
  let closed = false;
  let wake: (() => void) | undefined;
  const signal = () => {
    const current = wake;
    wake = undefined;
    current?.();
  };
  return {
    stream: (async function* () {
      for (;;) {
        const next = queue.shift();
        if (next) {
          yield next;
          continue;
        }
        if (closed) return;
        await new Promise<void>((resolve) => {
          wake = resolve;
        });
      }
    })(),
    push: (text) => {
      if (closed) throw new Error("Claude Agent SDK prompt stream is closed.");
      queue.push(claudeUserMessage(text, "now"));
      signal();
    },
    close: () => {
      if (closed) return;
      closed = true;
      signal();
    },
  };
}

function claudeUserMessage(text: string, priority?: "now"): SDKUserMessage {
  return {
    type: "user",
    session_id: "",
    parent_tool_use_id: null,
    message: {
      role: "user",
      content: [{ type: "text", text }],
    },
    ...(priority ? { priority } : {}),
  } as unknown as SDKUserMessage;
}

function registerAbort(signal: AbortSignal, onAbort: () => void): () => void {
  let active = true;
  const handler = () => {
    if (!active) return;
    active = false;
    signal.removeEventListener("abort", handler);
    onAbort();
  };
  signal.addEventListener("abort", handler, { once: true });
  if (signal.aborted) handler();
  return () => {
    active = false;
    signal.removeEventListener("abort", handler);
  };
}

function remapAnswers(
  questions: ReadonlyArray<UserInputQuestion>,
  answers: Record<string, unknown>,
): Record<string, string> {
  const remapped: Record<string, string> = {};
  for (const [key, value] of Object.entries(answers)) {
    remapped[key] = answerText(value);
  }
  for (const question of questions) {
    if (Object.hasOwn(remapped, question.question)) continue;
    if (!Object.hasOwn(remapped, question.id)) continue;
    remapped[question.question] = remapped[question.id] ?? "";
    delete remapped[question.id];
  }
  return remapped;
}

function answerText(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) {
    return value.filter((entry): entry is string => typeof entry === "string").join(", ");
  }
  return "";
}

function classifyRequestKind(toolName: string): string {
  const normalized = toolName.toLowerCase();
  if (normalized === "bash" || normalized.includes("command")) return "command";
  if (normalized === "read" || normalized.includes("read")) return "file-read";
  if (
    normalized === "write" ||
    normalized === "edit" ||
    normalized.includes("write") ||
    normalized.includes("edit")
  ) {
    return "file-change";
  }
  if (normalized.includes("web") || normalized.includes("network")) return "network";
  return "tool";
}

function approvalSummary(requestKind: string, toolName: string): string {
  switch (requestKind) {
    case "command":
      return "Claude wants to run a command.";
    case "file-read":
      return "Claude wants to read a file.";
    case "file-change":
      return "Claude wants to change a file.";
    case "network":
      return "Claude wants to access the network.";
    default:
      return `Claude wants to use ${toolName}.`;
  }
}

function isClientSurfacedTool(toolName: string): boolean {
  return toolName === "AskUserQuestion" || toolName === "ExitPlanMode";
}

function isReadOnlyTool(toolName: string): boolean {
  const normalized = toolName.toLowerCase();
  return (
    normalized === "read" ||
    normalized === "glob" ||
    normalized === "grep" ||
    normalized.includes("read file") ||
    normalized.includes("search files")
  );
}

function resultErrorMessage(result: Record<string, unknown>): string {
  if (Array.isArray(result.errors)) {
    const errors = result.errors.filter((value): value is string => typeof value === "string");
    if (errors.length > 0) return errors.join("\n");
  }
  const subtype = readString(result, "subtype") ?? "error_during_execution";
  return `Claude Agent SDK returned ${subtype}.`;
}

function isResumeFailure(message: string): boolean {
  const normalized = message.toLowerCase();
  return (
    normalized.includes("no conversation found") ||
    normalized.includes("session not found") ||
    normalized.includes("invalid session") ||
    normalized.includes("expired session") ||
    (normalized.includes("resume") &&
      (normalized.includes("invalid") ||
        normalized.includes("expired") ||
        normalized.includes("not found") ||
        normalized.includes("does not exist")))
  );
}

function numericFields(value: Record<string, unknown>): Record<string, number> {
  return Object.fromEntries(
    Object.entries(value).filter((entry): entry is [string, number] => typeof entry[1] === "number"),
  );
}

function boundedString(value: unknown, maximumLength: number): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed ? trimmed.slice(0, maximumLength) : undefined;
}

function trimmedString(value: unknown): string | undefined {
  return boundedString(value, Number.MAX_SAFE_INTEGER);
}

function requiredString(value: unknown, label: string): string {
  const normalized = trimmedString(value);
  if (!normalized) throw new Error(`${label} is required`);
  return normalized;
}

function readString(
  value: Record<string, unknown> | undefined,
  key: string,
): string | undefined {
  return value && typeof value[key] === "string" ? value[key] : undefined;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}
