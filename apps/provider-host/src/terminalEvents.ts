import { realpathSync } from "node:fs";
import path from "node:path";

import type { RunnerMessage } from "./providerHost";

const TERMINAL_OUTPUT_CHUNK_BYTES = 8 * 1024;
const TERMINAL_COMMAND_SUMMARY_LENGTH = 1_000;
const TERMINAL_CWD_LABEL_LENGTH = 500;

type RunnerEmitter = (message: RunnerMessage) => void;

export type TerminalRedactor = ((value: string) => string) & {
  readonly secretValues?: ReadonlyArray<string>;
};

export type TerminalOutputStream = {
  write: (output: string) => void;
  flush: () => void;
  bytesWritten: () => number;
};

export function terminalCommandSummary(
  value: unknown,
  redact: (value: string) => string,
): string | undefined {
  const command = Array.isArray(value)
    ? value.filter((part): part is string => typeof part === "string").join(" ")
    : typeof value === "string"
      ? value
      : undefined;
  if (!command) return undefined;
  const firstLine = command.split(/\r?\n/u, 1)[0] ?? "";
  const redacted = redact(firstLine)
    .replaceAll(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/gu, "?")
    .replaceAll(
      /\b([a-z0-9_]*(?:api_?key|auth|credential|pass(?:word|wd)?|secret|token)[a-z0-9_]*)=(?:"[^"]*"|'[^']*'|\S+)/giu,
      "$1=[REDACTED]",
    )
    .replaceAll(
      /(\B--?(?:api[-_]?key|auth(?:entication|orization)?|credential|pass(?:word|wd)?|secret|token)(?:=|\s+))(?:"[^"]*"|'[^']*'|\S+)/giu,
      "$1[REDACTED]",
    )
    .replaceAll(/(authorization\s*:\s*)(?:bearer\s+)?[^\s'";]+/giu, "$1[REDACTED]")
    .replaceAll(/(\b[a-z][a-z0-9+.-]*:\/\/[^:\s/@]+:)[^@\s]+@/giu, "$1[REDACTED]@")
    .replaceAll(/\s+/gu, " ")
    .trim();
  return redacted.length > 0 ? redacted.slice(0, TERMINAL_COMMAND_SUMMARY_LENGTH) : undefined;
}

export function terminalCwdLabel(workspaceDirectory: string, value: unknown): string | undefined {
  if (typeof value !== "string" || value.trim().length === 0) return undefined;
  const workspace = comparablePath(workspaceDirectory);
  const cwd = comparablePath(
    path.isAbsolute(value) ? value : path.resolve(workspaceDirectory, value),
  );
  const relative = path.relative(workspace, cwd);
  const label =
    relative === ""
      ? "."
      : !path.isAbsolute(relative) && relative !== ".." && !relative.startsWith(`..${path.sep}`)
        ? relative
        : path.basename(cwd);
  const normalized = label.split(path.sep).join("/").trim();
  return normalized.length > 0 ? normalized.slice(0, TERMINAL_CWD_LABEL_LENGTH) : undefined;
}

function comparablePath(value: string): string {
  const resolved = path.resolve(value);
  try {
    return realpathSync.native(resolved);
  } catch {
    return resolved;
  }
}

export function emitTerminalOutput(input: {
  emit: RunnerEmitter;
  provider: "codex" | "claudeAgent";
  terminalId: string;
  output: string;
  redact: TerminalRedactor;
}): number {
  const stream = createTerminalOutputStream(input);
  stream.write(input.output);
  stream.flush();
  return stream.bytesWritten();
}

export function createTerminalOutputStream(input: {
  emit: RunnerEmitter;
  provider: "codex" | "claudeAgent";
  terminalId: string;
  redact: TerminalRedactor;
}): TerminalOutputStream {
  const secrets = input.redact.secretValues ?? [];
  const maximumSecretLength = secrets.reduce(
    (maximum, secret) => Math.max(maximum, secret.length),
    0,
  );
  let pending = "";
  let byteOffset = 0;

  const emit = (value: string) => {
    const redacted = input.redact(value);
    if (redacted.length === 0) return;
    for (const text of chunkUtf8(redacted, TERMINAL_OUTPUT_CHUNK_BYTES)) {
      const byteLength = Buffer.byteLength(text, "utf8");
      input.emit({
        type: "event",
        eventType: "runtime.command.output",
        payload: {
          provider: input.provider,
          terminalId: input.terminalId,
          encoding: "utf-8",
          text,
          byteOffset,
          byteLength,
        },
      });
      byteOffset += byteLength;
    }
  };

  return {
    write(output) {
      if (output.length === 0) return;
      if (maximumSecretLength === 0) {
        emit(output);
        return;
      }
      const combined = pending + output;
      const initialCutoff = Math.max(0, combined.length - maximumSecretLength + 1);
      const cutoff = avoidSurrogateSplit(
        combined,
        safeRedactionPrefixLength(combined, initialCutoff, secrets),
      );
      emit(combined.slice(0, cutoff));
      pending = combined.slice(cutoff);
    },
    flush() {
      emit(pending);
      pending = "";
    },
    bytesWritten() {
      return byteOffset;
    },
  };
}

export function terminalResultText(value: unknown): string {
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return "";
  return value
    .map((entry) => {
      if (typeof entry === "string") return entry;
      if (!entry || typeof entry !== "object") return "";
      const record = entry as Record<string, unknown>;
      return typeof record.text === "string" ? record.text : "";
    })
    .filter((entry) => entry.length > 0)
    .join("\n");
}

function* chunkUtf8(value: string, maximumBytes: number): Generator<string> {
  let current = "";
  let currentBytes = 0;
  for (const character of value) {
    const characterBytes = Buffer.byteLength(character, "utf8");
    if (currentBytes > 0 && currentBytes + characterBytes > maximumBytes) {
      yield current;
      current = "";
      currentBytes = 0;
    }
    current += character;
    currentBytes += characterBytes;
  }
  if (current.length > 0) yield current;
}

function safeRedactionPrefixLength(
  value: string,
  initialCutoff: number,
  secrets: ReadonlyArray<string>,
): number {
  let cutoff = initialCutoff;
  let changed = true;
  while (changed && cutoff > 0) {
    changed = false;
    for (const secret of secrets) {
      let index = value.indexOf(secret, Math.max(0, cutoff - secret.length + 1));
      while (index >= 0 && index < cutoff) {
        if (index + secret.length > cutoff) {
          cutoff = index;
          changed = true;
          break;
        }
        index = value.indexOf(secret, index + 1);
      }
      if (changed) break;
    }
  }
  return cutoff;
}

function avoidSurrogateSplit(value: string, cutoff: number): number {
  if (cutoff <= 0 || cutoff >= value.length) return cutoff;
  const before = value.charCodeAt(cutoff - 1);
  const after = value.charCodeAt(cutoff);
  return before >= 0xd800 && before <= 0xdbff && after >= 0xdc00 && after <= 0xdfff
    ? cutoff - 1
    : cutoff;
}
