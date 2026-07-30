// FILE: codexProcessEnv.ts
// Purpose: Builds the exact environment used when Synara launches Codex subprocesses.
// Layer: Server runtime utility
// Exports: Codex process env builder and browser-plugin overlay helpers.
// Depends on: Codex home path helpers, shared Codex config parsing, login-shell env reader.

import { createHash } from "node:crypto";
import * as fs from "node:fs/promises";
import path from "node:path";

import { readActiveCodexProviderEnvKey } from "@synara/shared/codexConfig";
import {
  readEnvironmentFromLoginShell,
  resolveLoginShell,
  type ShellEnvironmentReader,
} from "@synara/shared/shell";

import { resolveBaseCodexHomePath, resolveSynaraCodexHomeOverlayPath } from "./codexHomePaths.ts";
import {
  importCodexSessionFiles,
  ISOLATED_CODEX_HOME_SHARED_SOURCE_ENTRIES,
  prepareIsolatedCodexHomeDirectories,
} from "./codexIsolatedHome.ts";
import { buildProviderChildEnvironment } from "./providerChildEnvironment.ts";

const CODEX_PROCESS_SHELL_ENV_NAMES = ["PATH", "SSH_AUTH_SOCK"] as const;
const SYNARA_CODEX_MODEL_PROVIDER_TOKEN_ENV_PREFIX = "SYNARA_CODEX_MODEL_PROVIDER_TOKEN_";
const NODE_REPL_SANDBOX_ALLOWED_UNIX_SOCKETS = "NODE_REPL_SANDBOX_ALLOWED_UNIX_SOCKETS";
const NODE_REPL_MCP_SERVER_HEADER = "[mcp_servers.node_repl]";
const CODEX_OVERLAY_SHARED_STATE_FILES = new Set(["auth.json"]);
const SYNARA_CONFIG_SUPPRESSIONS_FILE = "synara-config-suppressions-v1.json";
const SYNARA_MANAGED_MCP_TABLE_HEADER = "[mcp_servers.synara]";
export const SYNARA_COMPETING_BROWSER_PLUGIN_SECTION_HEADERS = [
  '[plugins."browser@openai-bundled"]',
  '[plugins."chrome@openai-bundled"]',
  '[plugins."computer-use@openai-bundled"]',
] as const;
const MAX_CONFIG_SUPPRESSION_SECTIONS = 32;
const MAX_CONFIG_SUPPRESSION_HEADER_LENGTH = 256;
const ISOLATED_CODEX_TOP_LEVEL_MODEL_PROVIDER_KEYS = new Set([
  "chatgpt_base_url",
  "model_provider",
  "openai_base_url",
]);
const ISOLATED_CODEX_MODEL_PROVIDER_KEYS = new Set([
  "base_url",
  "env_http_headers",
  "env_key",
  "env_key_instructions",
  "name",
  "request_max_retries",
  "requires_openai_auth",
  "stream_idle_timeout_ms",
  "stream_max_retries",
  "supports_standalone_web_search",
  "supports_websockets",
  "websocket_connect_timeout_ms",
  "wire_api",
]);
const codexOverlayPreparationQueues = new Map<string, Promise<void>>();
// Retired local browser integrations used a stable six-character namespace.
// Match the structural conflict without retaining any previous product name.
const CONFLICTING_LOCAL_BROWSER_PLUGIN_SECTION_PATTERN =
  /^\[plugins\."[a-z0-9][a-z0-9-]{5}-browser@local"\]$/;

interface CodexOverlayEntryLinker {
  readonly symlink: typeof fs.symlink;
  readonly copyFile: typeof fs.copyFile;
}

function isSafePluginSectionHeader(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length <= MAX_CONFIG_SUPPRESSION_HEADER_LENGTH &&
    /^\[plugins\."[^"\r\n]+"\]$/.test(value)
  );
}

export async function readSynaraConfigSuppressions(markerPath: string): Promise<readonly string[]> {
  try {
    const parsed = JSON.parse(await fs.readFile(markerPath, "utf8")) as unknown;
    if (typeof parsed !== "object" || parsed === null) return [];
    const marker = parsed as { version?: unknown; sectionHeaders?: unknown };
    if (marker.version !== 1 || !Array.isArray(marker.sectionHeaders)) return [];
    if (marker.sectionHeaders.length > MAX_CONFIG_SUPPRESSION_SECTIONS) return [];
    return [...new Set(marker.sectionHeaders.filter(isSafePluginSectionHeader))];
  } catch {
    return [];
  }
}

function findConflictingLocalBrowserPluginSections(config: string): readonly string[] {
  return [
    ...new Set(
      config
        .split(/\r?\n/)
        .map((line) => line.trim())
        .filter((line) => CONFLICTING_LOCAL_BROWSER_PLUGIN_SECTION_PATTERN.test(line)),
    ),
  ];
}

export function disableCodexConfigSections(
  config: string,
  sectionHeaders: readonly string[],
  appendMissing = false,
): string {
  const targetsByName = new Map<string, string>();
  for (const header of sectionHeaders) {
    if (!isSafePluginSectionHeader(header)) continue;
    const tableName = normalizeTomlTableHeaderName(header);
    if (tableName !== undefined && !targetsByName.has(tableName)) {
      targetsByName.set(tableName, header);
    }
  }
  const lines = config.split(/\r?\n/);
  const output: string[] = [];
  let inTargetSection = false;
  const seenTargetSections = new Set<string>();
  let targetSectionHasEnabled = false;

  const closeTargetSection = () => {
    if (inTargetSection && !targetSectionHasEnabled) {
      output.push("enabled = false");
    }
  };

  for (const line of lines) {
    const tableName = normalizeTomlTableHeaderName(line);
    if (tableName !== undefined) {
      closeTargetSection();
      inTargetSection = targetsByName.has(tableName);
      if (inTargetSection) seenTargetSections.add(tableName);
      targetSectionHasEnabled = false;
      output.push(line);
      continue;
    }

    if (inTargetSection && /^\s*enabled\s*=/.test(line)) {
      output.push("enabled = false");
      targetSectionHasEnabled = true;
      continue;
    }

    output.push(line);
  }

  closeTargetSection();

  if (appendMissing) {
    for (const [tableName, header] of targetsByName) {
      if (seenTargetSections.has(tableName)) continue;
      if (output.length > 0 && output.at(-1)?.trim()) {
        output.push("");
      }
      output.push(header, "enabled = false");
    }
  }

  return output.join("\n");
}

async function writeSynaraConfigSuppressions(
  markerPath: string,
  sectionHeaders: readonly string[],
): Promise<void> {
  const normalized = [...new Set(sectionHeaders.filter(isSafePluginSectionHeader))].slice(
    0,
    MAX_CONFIG_SUPPRESSION_SECTIONS,
  );
  const temporaryPath = `${markerPath}.${process.pid}.tmp`;
  await fs.writeFile(
    temporaryPath,
    `${JSON.stringify({ version: 1, sectionHeaders: normalized }, null, 2)}\n`,
    { encoding: "utf8", mode: 0o600 },
  );
  await fs.rename(temporaryPath, markerPath);
}

async function writeCodexOverlayConfig(configPath: string, config: string): Promise<void> {
  const temporaryPath = `${configPath}.${process.pid}.tmp`;
  await fs.writeFile(temporaryPath, config, { encoding: "utf8", mode: 0o600 });
  await fs.rename(temporaryPath, configPath);
}

export async function linkOrCopyCodexOverlayEntry(
  input: {
    readonly entryName: string;
    readonly sourcePath: string;
    readonly targetPath: string;
    readonly type: "dir" | "file";
  },
  linker: CodexOverlayEntryLinker = {
    symlink: fs.symlink,
    copyFile: fs.copyFile,
  },
): Promise<void> {
  try {
    await linker.symlink(input.sourcePath, input.targetPath, input.type);
  } catch (error: unknown) {
    if (input.type === "file" && CODEX_OVERLAY_SHARED_STATE_FILES.has(input.entryName)) {
      await linker.copyFile(input.sourcePath, input.targetPath);
      return;
    }
    throw error;
  }
}

export function prioritizeCodexOverlayEntries(entries: readonly string[]): string[] {
  const sharedStateEntries: string[] = [];
  const otherEntries: string[] = [];

  for (const entry of entries) {
    if (CODEX_OVERLAY_SHARED_STATE_FILES.has(entry)) {
      sharedStateEntries.push(entry);
    } else {
      otherEntries.push(entry);
    }
  }

  return [...sharedStateEntries, ...otherEntries];
}

async function ensureCodexOverlaySymlink(input: {
  readonly entryName: string;
  readonly sourcePath: string;
  readonly targetPath: string;
  readonly type: "dir" | "file";
}): Promise<void> {
  let targetStat: Awaited<ReturnType<typeof fs.lstat>> | undefined;
  try {
    targetStat = await fs.lstat(input.targetPath);
  } catch {
    targetStat = undefined;
  }

  if (targetStat) {
    if (targetStat.isSymbolicLink() && (await fs.readlink(input.targetPath)) === input.sourcePath) {
      return;
    }

    if (
      targetStat.isSymbolicLink() ||
      /^.+\.sqlite(?:-(?:wal|shm|journal))?$/.test(input.entryName) ||
      CODEX_OVERLAY_SHARED_STATE_FILES.has(input.entryName)
    ) {
      // SQLite files must stay generation-matched, and auth must mirror the
      // user's real Codex home so external `codex login` changes are visible.
      await fs.rm(input.targetPath, { recursive: true, force: true });
    } else {
      return;
    }
  }

  await linkOrCopyCodexOverlayEntry(input);
}

export function appendCodexConfigSection(config: string, section: string): string {
  const trimmedSection = section.trim();
  if (!trimmedSection) {
    return config;
  }
  if (config.includes(trimmedSection.split("\n")[0] ?? trimmedSection)) {
    return config;
  }
  const base = config.trimEnd();
  return base.length > 0 ? `${base}\n\n${trimmedSection}\n` : `${trimmedSection}\n`;
}

export const SYNARA_MANAGED_CODEX_CONFIG_BEGIN = "# >>> synara managed config >>>";
export const SYNARA_MANAGED_CODEX_CONFIG_END = "# <<< synara managed config <<<";

export function extractManagedCodexConfigSection(config: string): string | undefined {
  const begin = config.indexOf(SYNARA_MANAGED_CODEX_CONFIG_BEGIN);
  if (begin === -1) {
    return undefined;
  }
  const contentStart = begin + SYNARA_MANAGED_CODEX_CONFIG_BEGIN.length;
  const end = config.indexOf(SYNARA_MANAGED_CODEX_CONFIG_END, contentStart);
  if (end === -1) {
    return undefined;
  }
  const content = config.slice(contentStart, end).trim();
  return content.length > 0 ? content : undefined;
}

function normalizeTomlTableHeaderName(line: string): string | undefined {
  const match = /^\s*\[\s*(.*?)\s*\]\s*(?:#.*)?$/.exec(line);
  if (!match) {
    return undefined;
  }
  const tableName = match[1];
  if (tableName === undefined) {
    return undefined;
  }
  const parts: string[] = [];
  let index = 0;
  const skipWhitespace = () => {
    while (index < tableName.length && /[\t ]/.test(tableName[index]!)) index += 1;
  };
  const parseBasicQuotedKey = (): string | undefined => {
    index += 1;
    let value = "";
    while (index < tableName.length) {
      const character = tableName[index++]!;
      if (character === '"') return value;
      if (character !== "\\") {
        if (character.charCodeAt(0) < 0x20) return undefined;
        value += character;
        continue;
      }
      const escape = tableName[index++];
      const simpleEscapes: Readonly<Record<string, string>> = {
        b: "\b",
        t: "\t",
        n: "\n",
        f: "\f",
        r: "\r",
        '"': '"',
        "\\": "\\",
      };
      if (escape !== undefined && simpleEscapes[escape] !== undefined) {
        value += simpleEscapes[escape];
        continue;
      }
      if (escape !== "u" && escape !== "U") return undefined;
      const length = escape === "u" ? 4 : 8;
      const hexadecimal = tableName.slice(index, index + length);
      if (!new RegExp(`^[0-9A-Fa-f]{${length}}$`).test(hexadecimal)) return undefined;
      const codePoint = Number.parseInt(hexadecimal, 16);
      if (codePoint > 0x10ffff || (codePoint >= 0xd800 && codePoint <= 0xdfff)) return undefined;
      value += String.fromCodePoint(codePoint);
      index += length;
    }
    return undefined;
  };
  const parseLiteralQuotedKey = (): string | undefined => {
    index += 1;
    const end = tableName.indexOf("'", index);
    if (end === -1) return undefined;
    const value = tableName.slice(index, end);
    index = end + 1;
    return value;
  };

  while (index < tableName.length) {
    skipWhitespace();
    let part: string | undefined;
    if (tableName[index] === '"') {
      part = parseBasicQuotedKey();
    } else if (tableName[index] === "'") {
      part = parseLiteralQuotedKey();
    } else {
      const start = index;
      while (index < tableName.length && /[A-Za-z0-9_-]/.test(tableName[index]!)) index += 1;
      part = index > start ? tableName.slice(start, index) : undefined;
    }
    if (part === undefined) return undefined;
    parts.push(part);
    skipWhitespace();
    if (index === tableName.length) break;
    if (tableName[index] !== ".") return undefined;
    index += 1;
    skipWhitespace();
    if (index === tableName.length) return undefined;
  }
  return parts.length > 0 ? JSON.stringify(parts) : undefined;
}

interface TomlTableHeaderLocation {
  readonly index: number;
  readonly end: number;
}

function findTomlTableHeader(config: string, header: string): TomlTableHeaderLocation | undefined {
  const target = normalizeTomlTableHeaderName(header);
  if (!target) {
    return undefined;
  }
  let offset = 0;
  for (const rawLine of config.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (normalizeTomlTableHeaderName(line) === target) {
      return { index: offset, end: offset + line.length };
    }
    offset += rawLine.length + 1;
  }
  return undefined;
}

function findNextTomlTableHeaderIndex(config: string, start: number): number {
  const tail = config.slice(start);
  let offset = 0;
  for (const rawLine of tail.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (normalizeTomlTableHeaderName(line) !== undefined) {
      return start + offset;
    }
    offset += rawLine.length + 1;
  }
  return config.length;
}

export function configHasTomlTableHeader(config: string, header: string): boolean {
  return findTomlTableHeader(config, header) !== undefined;
}

function readSingleLineTomlStringAssignment(line: string): string | undefined {
  const value = line.slice(line.indexOf("=") + 1).trim();
  const match = /^("(?:[^"\\]|\\.)*"|'[^']*')/u.exec(value)?.[1];
  if (!match) return undefined;
  if (match.startsWith("'")) return match.slice(1, -1);
  try {
    return JSON.parse(match) as string;
  } catch {
    return undefined;
  }
}

function sanitizedCodexModelProviderTokenEnvName(providerName: string): string {
  const digest = createHash("sha256").update(providerName, "utf8").digest("hex").slice(0, 16);
  return `${SYNARA_CODEX_MODEL_PROVIDER_TOKEN_ENV_PREFIX}${digest.toUpperCase()}`;
}

function assertSafeIsolatedCodexProviderBaseUrl(value: string): void {
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error(
      "Codex model provider base URL must be an absolute HTTP(S) URL in isolated mode.",
    );
  }
  if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || !parsed.hostname) {
    throw new Error(
      "Codex model provider base URL must be an absolute HTTP(S) URL in isolated mode.",
    );
  }
  if (
    parsed.username ||
    parsed.password ||
    parsed.search ||
    parsed.hash ||
    value.includes("?") ||
    value.includes("#")
  ) {
    throw new Error(
      "Codex model provider base URL cannot contain credentials, query parameters, or fragments in isolated mode.",
    );
  }
}

function assertSafeIsolatedCodexProviderQueryParam(line: string): void {
  if (!/^(?:api-version|"api-version"|'api-version')\s*=/u.test(line)) {
    throw new Error(
      "Codex model provider query_params may contain only an explicit api-version in isolated mode.",
    );
  }
  const value = readSingleLineTomlStringAssignment(line);
  if (!value || !/^\d{4}-\d{2}-\d{2}(?:-preview)?$/u.test(value)) {
    throw new Error(
      "Codex model provider api-version must be a date or date-preview value in isolated mode.",
    );
  }
}

/**
 * Retains model transport configuration without carrying executable provider
 * auth commands, hooks, MCP servers, plugins, profiles, or project trust.
 */
function sanitizeCodexModelProviderConfigAndSecrets(config: string): {
  readonly config: string;
  readonly secretEnvironment: Readonly<Record<string, string>>;
  readonly credentialEnvironmentNames: ReadonlyArray<string>;
} {
  type Section =
    | "top-level"
    | "model-provider"
    | "model-provider-env-headers"
    | "model-provider-query-params"
    | "ignored";
  const lines = config.split(/\r?\n/u);
  const providersWithEnvKey = new Set<string>();
  let scannedProviderName: string | undefined;
  for (const rawLine of lines) {
    const trimmed = rawLine.trim();
    if (trimmed.startsWith("[") && trimmed.endsWith("]")) {
      const normalized = normalizeTomlTableHeaderName(trimmed);
      const parts = normalized ? (JSON.parse(normalized) as string[]) : [];
      scannedProviderName =
        parts[0] === "model_providers" && parts.length === 2 ? parts[1] : undefined;
      continue;
    }
    if (scannedProviderName && /^env_key\s*=/u.test(trimmed)) {
      providersWithEnvKey.add(scannedProviderName);
    }
  }

  let section: Section = "top-level";
  let providerName: string | undefined;
  const output: string[] = [];
  const secretEnvironment: Record<string, string> = {};

  for (const rawLine of lines) {
    const trimmed = rawLine.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;

    if (trimmed.startsWith("[") && trimmed.endsWith("]")) {
      const normalized = normalizeTomlTableHeaderName(trimmed);
      const parts = normalized ? (JSON.parse(normalized) as string[]) : [];
      if (parts[0] === "model_providers" && parts.length === 2) {
        section = "model-provider";
        providerName = parts[1];
        output.push(trimmed);
      } else if (
        parts[0] === "model_providers" &&
        parts.length === 3 &&
        parts[2] === "env_http_headers"
      ) {
        section = "model-provider-env-headers";
        providerName = parts[1];
        output.push(trimmed);
      } else if (
        parts[0] === "model_providers" &&
        parts.length === 3 &&
        parts[2] === "query_params"
      ) {
        section = "model-provider-query-params";
        providerName = parts[1];
        output.push(trimmed);
      } else {
        section = "ignored";
        providerName = undefined;
      }
      continue;
    }

    if (section === "model-provider-env-headers") {
      output.push(trimmed);
      continue;
    }
    if (section === "model-provider-query-params") {
      assertSafeIsolatedCodexProviderQueryParam(trimmed);
      output.push(trimmed);
      continue;
    }

    const assignment = /^([A-Za-z0-9_-]+)\s*=/u.exec(trimmed);
    const key = assignment?.[1];
    if (section === "model-provider" && key === "query_params") {
      throw new Error(
        "Codex model provider query_params must use a dedicated TOML table in isolated mode.",
      );
    }
    if (
      (section === "top-level" && (key === "chatgpt_base_url" || key === "openai_base_url")) ||
      (section === "model-provider" && key === "base_url")
    ) {
      const baseUrl = readSingleLineTomlStringAssignment(trimmed);
      if (!baseUrl) {
        throw new Error(
          "Codex model provider base URL must be a single-line TOML string in isolated mode.",
        );
      }
      assertSafeIsolatedCodexProviderBaseUrl(baseUrl);
    }
    if (
      section === "model-provider" &&
      key === "experimental_bearer_token" &&
      providerName &&
      !providersWithEnvKey.has(providerName)
    ) {
      const token = readSingleLineTomlStringAssignment(trimmed);
      if (!token) {
        throw new Error(
          "Codex model provider bearer token must be a single-line TOML string in isolated mode.",
        );
      }
      const envName = sanitizedCodexModelProviderTokenEnvName(providerName);
      secretEnvironment[envName] = token;
      output.push(`env_key = ${JSON.stringify(envName)}`);
      continue;
    }
    if (
      (section === "top-level" &&
        key !== undefined &&
        ISOLATED_CODEX_TOP_LEVEL_MODEL_PROVIDER_KEYS.has(key)) ||
      (section === "model-provider" &&
        key !== undefined &&
        ISOLATED_CODEX_MODEL_PROVIDER_KEYS.has(key))
    ) {
      output.push(trimmed);
    }
  }

  const sanitizedConfig = output.length > 0 ? `${output.join("\n")}\n` : "";
  return {
    config: sanitizedConfig,
    secretEnvironment,
    credentialEnvironmentNames: codexModelProviderCredentialEnvNames(sanitizedConfig),
  };
}

export function sanitizeCodexModelProviderConfig(config: string): string {
  return sanitizeCodexModelProviderConfigAndSecrets(config).config;
}

export function codexModelProviderCredentialEnvNames(config: string): ReadonlyArray<string> {
  let section: "model-provider" | "env-http-headers" | undefined;
  const names = new Set<string>();
  for (const rawLine of config.split(/\r?\n/u)) {
    const trimmed = rawLine.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    if (trimmed.startsWith("[") && trimmed.endsWith("]")) {
      const normalized = normalizeTomlTableHeaderName(trimmed);
      const parts = normalized ? (JSON.parse(normalized) as string[]) : [];
      section =
        parts[0] === "model_providers" && parts.length === 2
          ? "model-provider"
          : parts[0] === "model_providers" && parts.length === 3 && parts[2] === "env_http_headers"
            ? "env-http-headers"
            : undefined;
      continue;
    }
    if (section === "model-provider" && /^env_http_headers\s*=/u.test(trimmed)) {
      throw new Error(
        "Codex model provider env_http_headers must use a dedicated TOML table in isolated mode.",
      );
    }
    if (
      (section === "model-provider" && /^env_key\s*=/u.test(trimmed)) ||
      (section === "env-http-headers" && trimmed.includes("="))
    ) {
      const name = readSingleLineTomlStringAssignment(trimmed);
      if (!name || !/^[A-Za-z_][A-Za-z0-9_]*$/u.test(name)) {
        throw new Error(
          section === "model-provider"
            ? "Codex model provider env_key must be a portable environment variable name in isolated mode."
            : "Codex model provider env_http_headers values must be portable environment variable names in isolated mode.",
        );
      }
      names.add(name);
    }
  }
  return [...names].sort();
}

function splitTomlTables(snippet: string): string[] {
  const tables: string[] = [];
  let current: string[] = [];
  for (const line of snippet.split("\n")) {
    if (/^\s*\[/.test(line) && current.length > 0) {
      tables.push(current.join("\n").trim());
      current = [];
    }
    current.push(line);
  }
  if (current.length > 0) {
    tables.push(current.join("\n").trim());
  }
  return tables.filter((table) => table.length > 0);
}

function findTomlTableHeaderInNamespace(
  config: string,
  namespaceHeader: string,
): TomlTableHeaderLocation | undefined {
  const namespace = normalizeTomlTableHeaderName(namespaceHeader);
  if (!namespace) {
    return undefined;
  }
  const descendantPrefix = `${namespace.slice(0, -1)},`;
  let offset = 0;
  for (const rawLine of config.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    const table = normalizeTomlTableHeaderName(line);
    if (table === namespace || table?.startsWith(descendantPrefix)) {
      return { index: offset, end: offset + line.length };
    }
    offset += rawLine.length + 1;
  }
  return undefined;
}

function removeTomlTableNamespace(config: string, namespaceHeader: string): string {
  let result = config;
  while (true) {
    const match = findTomlTableHeaderInNamespace(result, namespaceHeader);
    if (!match) {
      return result;
    }
    const tableEnd = findNextTomlTableHeaderIndex(result, match.end);
    result = `${result.slice(0, match.index)}${result.slice(tableEnd)}`;
  }
}

function maskTomlComments(input: string): string {
  let result = "";
  let quote: '"' | "'" | undefined;
  let escaped = false;
  let inComment = false;

  for (const character of input) {
    if (inComment) {
      if (character === "\n" || character === "\r") {
        inComment = false;
        result += character;
      } else {
        result += " ";
      }
      continue;
    }

    if (quote) {
      result += character;
      if (quote === '"' && escaped) {
        escaped = false;
      } else if (quote === '"' && character === "\\") {
        escaped = true;
      } else if (character === quote) {
        quote = undefined;
      }
      continue;
    }

    if (character === '"' || character === "'") {
      quote = character;
      result += character;
    } else if (character === "#") {
      inComment = true;
      result += " ";
    } else {
      result += character;
    }
  }

  return result;
}

function findTomlArrayEnd(input: string, openBracketIndex: number): number | undefined {
  let quote: '"' | "'" | undefined;
  let escaped = false;
  let depth = 0;

  for (let index = openBracketIndex; index < input.length; index += 1) {
    const character = input[index];
    if (quote) {
      if (quote === '"' && escaped) {
        escaped = false;
      } else if (quote === '"' && character === "\\") {
        escaped = true;
      } else if (character === quote) {
        quote = undefined;
      }
      continue;
    }

    if (character === '"' || character === "'") {
      quote = character;
    } else if (character === "[") {
      depth += 1;
    } else if (character === "]") {
      depth -= 1;
      if (depth === 0) {
        return index;
      }
    }
  }

  return undefined;
}

function mergeTomlStringArrayValues(
  config: string,
  tableHeader: string,
  key: string,
  values: readonly string[],
): string {
  const additions = [...new Set(values.filter(Boolean))];
  if (additions.length === 0) {
    return config;
  }
  const headerMatch = findTomlTableHeader(config, tableHeader);
  if (!headerMatch) {
    return config;
  }
  const tableStart = headerMatch.end;
  const tableEnd = findNextTomlTableHeaderIndex(config, tableStart);
  const tableBody = config.slice(tableStart, tableEnd);
  const activeTableBody = maskTomlComments(tableBody);
  const escapedKey = key.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const arrayPattern = new RegExp(`(^[\\t ]*${escapedKey}[\\t ]*=[\\t ]*\\[)`, "m");
  const arrayMatch = arrayPattern.exec(activeTableBody);

  if (arrayMatch) {
    const openBracketIndex = arrayMatch.index + arrayMatch[0].lastIndexOf("[");
    const closeBracketIndex = findTomlArrayEnd(activeTableBody, openBracketIndex);
    if (closeBracketIndex === undefined) {
      return config;
    }
    const activeArray = activeTableBody.slice(openBracketIndex + 1, closeBracketIndex);
    const missing = additions.filter((value) => {
      const escapedValue = value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      return !new RegExp(`(["'])${escapedValue}\\1`).test(activeArray);
    });
    if (missing.length === 0) {
      return config;
    }

    const insertAt = tableStart + openBracketIndex + 1;
    const separator = activeArray.trim().length > 0 ? ", " : "";
    return `${config.slice(0, insertAt)}${missing.map((value) => JSON.stringify(value)).join(", ")}${separator}${config.slice(insertAt)}`;
  }

  return `${config.slice(0, tableStart)}\n${key} = [${additions.map((value) => JSON.stringify(value)).join(", ")}]${config.slice(tableStart)}`;
}

export function mergeShellEnvPolicyExclude(config: string, envVarName: string): string {
  if (envVarName && !configHasTomlTableHeader(config, "[shell_environment_policy]")) {
    return appendCodexConfigSection(
      config,
      `[shell_environment_policy]\nexclude = [${JSON.stringify(envVarName)}]`,
    );
  }
  return mergeTomlStringArrayValues(
    config,
    "[shell_environment_policy]",
    "exclude",
    envVarName ? [envVarName] : [],
  );
}

function appendManagedCodexConfigSection(config: string, section: string): string {
  let overlayConfig = config;
  const managedMcpTableName = normalizeTomlTableHeaderName(SYNARA_MANAGED_MCP_TABLE_HEADER);
  const tables: string[] = [];

  for (const table of splitTomlTables(section.trim())) {
    const header = table.split("\n")[0]?.trim();
    if (header === undefined) {
      tables.push(table);
      continue;
    }
    if (normalizeTomlTableHeaderName(header) === managedMcpTableName) {
      // The session-scoped gateway entry is authoritative inside Synara's
      // overlay. The user's source config remains untouched.
      overlayConfig = removeTomlTableNamespace(overlayConfig, SYNARA_MANAGED_MCP_TABLE_HEADER);
      tables.push(table);
      continue;
    }
    if (!configHasTomlTableHeader(overlayConfig, header)) {
      tables.push(table);
    }
  }

  if (tables.length === 0) {
    return overlayConfig;
  }
  return appendCodexConfigSection(
    overlayConfig,
    `${SYNARA_MANAGED_CODEX_CONFIG_BEGIN}\n${tables.join("\n\n")}\n${SYNARA_MANAGED_CODEX_CONFIG_END}`,
  );
}

async function serializeCodexOverlayPreparation<A>(
  overlayHomePath: string,
  prepare: () => Promise<A>,
): Promise<A> {
  const previous = codexOverlayPreparationQueues.get(overlayHomePath) ?? Promise.resolve();
  const current = previous.catch(() => undefined).then(prepare);
  const queued = current.then(
    () => undefined,
    () => undefined,
  );
  codexOverlayPreparationQueues.set(overlayHomePath, queued);
  try {
    return await current;
  } finally {
    if (codexOverlayPreparationQueues.get(overlayHomePath) === queued) {
      codexOverlayPreparationQueues.delete(overlayHomePath);
    }
  }
}

async function prepareSynaraCodexHomeOverlayUnlocked(input: {
  readonly env: NodeJS.ProcessEnv;
  readonly homePath?: string;
  readonly appendConfigToml?: string;
  readonly isolateExecutableConfig?: boolean;
  readonly importSessionThreadIds?: ReadonlyArray<string>;
}): Promise<{
  readonly overlayHomePath?: string;
  readonly inheritedSynaraKeys: ReadonlyArray<string>;
}> {
  const sourceHomePath = resolveBaseCodexHomePath(input.env, input.homePath);
  const overlayHomePath = resolveSynaraCodexHomeOverlayPath(
    input.env,
    sourceHomePath,
    input.isolateExecutableConfig === undefined
      ? undefined
      : { isolateExecutableConfig: input.isolateExecutableConfig },
  );
  if (path.resolve(sourceHomePath) === path.resolve(overlayHomePath)) {
    if (input.isolateExecutableConfig) {
      throw new Error("Codex executable-config isolation requires a distinct source home.");
    }
    return { inheritedSynaraKeys: [] };
  }

  await fs.mkdir(overlayHomePath, { recursive: true });
  if (input.isolateExecutableConfig) {
    await prepareIsolatedCodexHomeDirectories(overlayHomePath);
  }

  try {
    // Auth must get a best-effort link/copy before optional entries whose
    // symlinks may fail on restricted Windows installs.
    for (const entry of prioritizeCodexOverlayEntries(await fs.readdir(sourceHomePath))) {
      if (
        entry === "config.toml" ||
        (input.isolateExecutableConfig && !ISOLATED_CODEX_HOME_SHARED_SOURCE_ENTRIES.has(entry))
      ) {
        continue;
      }
      const sourcePath = path.join(sourceHomePath, entry);
      const targetPath = path.join(overlayHomePath, entry);
      const stat = await fs.lstat(sourcePath);
      await ensureCodexOverlaySymlink({
        entryName: entry,
        sourcePath,
        targetPath,
        type: stat.isDirectory() ? "dir" : "file",
      });
    }
  } catch {
    // If the source home is partially missing, Codex can still start with the
    // overlay config and create any required state lazily.
  }

  if (input.isolateExecutableConfig && input.importSessionThreadIds?.length) {
    await importCodexSessionFiles({
      sourceHomePath,
      overlayHomePath,
      threadIds: input.importSessionThreadIds,
    });
  }

  const rawSourceConfig = await fs
    .readFile(path.join(sourceHomePath, "config.toml"), "utf8")
    .catch((cause: unknown) => {
      if ((cause as NodeJS.ErrnoException).code === "ENOENT") {
        return "";
      }
      throw cause;
    });
  const sanitizedModelProviderConfig = input.isolateExecutableConfig
    ? sanitizeCodexModelProviderConfigAndSecrets(rawSourceConfig)
    : undefined;
  const sourceConfig = sanitizedModelProviderConfig?.config ?? rawSourceConfig;
  for (const [name, value] of Object.entries(
    sanitizedModelProviderConfig?.secretEnvironment ?? {},
  )) {
    input.env[name] = value;
  }
  const suppressionMarkerPath = path.join(overlayHomePath, SYNARA_CONFIG_SUPPRESSIONS_FILE);
  const suppressedSections = input.isolateExecutableConfig
    ? []
    : [
        ...new Set([
          ...SYNARA_COMPETING_BROWSER_PLUGIN_SECTION_HEADERS,
          ...findConflictingLocalBrowserPluginSections(sourceConfig),
          ...(await readSynaraConfigSuppressions(suppressionMarkerPath)),
        ]),
      ].slice(0, MAX_CONFIG_SUPPRESSION_SECTIONS);
  const overlayConfigPath = path.join(overlayHomePath, "config.toml");
  let overlayConfig = disableCodexConfigSections(sourceConfig, suppressedSections, true);
  const managedSection =
    input.appendConfigToml ??
    (input.isolateExecutableConfig
      ? undefined
      : await fs
          .readFile(overlayConfigPath, "utf8")
          .then(extractManagedCodexConfigSection)
          .catch((cause: unknown) => {
            if ((cause as NodeJS.ErrnoException).code === "ENOENT") {
              return undefined;
            }
            throw cause;
          }));
  if (managedSection) {
    overlayConfig = appendManagedCodexConfigSection(overlayConfig, managedSection);
    const tokenEnvVar = /bearer_token_env_var\s*=\s*"([^"]+)"/.exec(managedSection)?.[1];
    if (tokenEnvVar) {
      overlayConfig = mergeShellEnvPolicyExclude(overlayConfig, tokenEnvVar);
    }
  }
  for (const tokenEnvVar of sanitizedModelProviderConfig?.credentialEnvironmentNames ?? []) {
    overlayConfig = mergeShellEnvPolicyExclude(overlayConfig, tokenEnvVar);
  }
  // Codex launches stdio MCP helpers with an environment allowlist, so the
  // Browser helper must opt in to the socket capability set on its parent.
  overlayConfig = mergeTomlStringArrayValues(
    overlayConfig,
    NODE_REPL_MCP_SERVER_HEADER,
    "env_vars",
    [NODE_REPL_SANDBOX_ALLOWED_UNIX_SOCKETS],
  );
  await writeCodexOverlayConfig(overlayConfigPath, overlayConfig);
  await writeSynaraConfigSuppressions(suppressionMarkerPath, suppressedSections);

  return {
    overlayHomePath,
    inheritedSynaraKeys: Object.keys(sanitizedModelProviderConfig?.secretEnvironment ?? {}),
  };
}

async function prepareSynaraCodexHomeOverlay(input: {
  readonly env: NodeJS.ProcessEnv;
  readonly homePath?: string;
  readonly appendConfigToml?: string;
  readonly isolateExecutableConfig?: boolean;
  readonly importSessionThreadIds?: ReadonlyArray<string>;
}): Promise<{
  readonly overlayHomePath?: string;
  readonly inheritedSynaraKeys: ReadonlyArray<string>;
}> {
  const sourceHomePath = resolveBaseCodexHomePath(input.env, input.homePath);
  const overlayHomePath = resolveSynaraCodexHomeOverlayPath(
    input.env,
    sourceHomePath,
    input.isolateExecutableConfig === undefined
      ? undefined
      : { isolateExecutableConfig: input.isolateExecutableConfig },
  );
  if (path.resolve(sourceHomePath) === path.resolve(overlayHomePath)) {
    if (input.isolateExecutableConfig) {
      throw new Error("Codex executable-config isolation requires a distinct source home.");
    }
    return { inheritedSynaraKeys: [] };
  }
  return serializeCodexOverlayPreparation(overlayHomePath, () =>
    prepareSynaraCodexHomeOverlayUnlocked(input),
  );
}

export async function buildCodexProcessEnv(
  input: {
    readonly env?: NodeJS.ProcessEnv;
    readonly homePath?: string;
    readonly platform?: NodeJS.Platform;
    readonly readEnvironment?: ShellEnvironmentReader;
    readonly appendConfigToml?: string;
    readonly isolateExecutableConfig?: boolean;
    readonly importSessionThreadIds?: ReadonlyArray<string>;
  } = {},
): Promise<NodeJS.ProcessEnv> {
  const baseEnv = { ...(input.env ?? process.env) };
  const preparedOverlay = await prepareSynaraCodexHomeOverlay({
    env: baseEnv,
    ...(input.homePath ? { homePath: input.homePath } : {}),
    ...(input.appendConfigToml ? { appendConfigToml: input.appendConfigToml } : {}),
    ...(input.isolateExecutableConfig ? { isolateExecutableConfig: true } : {}),
    ...(input.importSessionThreadIds?.length
      ? { importSessionThreadIds: input.importSessionThreadIds }
      : {}),
  });
  const overlayHomePath = preparedOverlay.overlayHomePath;
  const configuredEnv =
    overlayHomePath || input.homePath
      ? { ...baseEnv, CODEX_HOME: overlayHomePath ?? input.homePath }
      : baseEnv;
  const platform = input.platform ?? process.platform;
  const effectiveEnv = buildProviderChildEnvironment({
    provider: "codex",
    baseEnv: configuredEnv,
    inheritedSynaraKeys: preparedOverlay.inheritedSynaraKeys,
  });

  if (platform === "darwin" || platform === "linux") {
    try {
      const shell = resolveLoginShell(platform, effectiveEnv.SHELL);
      const providerEnvKey = readActiveCodexProviderEnvKey(effectiveEnv);
      if (shell && providerEnvKey && !effectiveEnv[providerEnvKey]?.trim()) {
        const shellEnvironment = (input.readEnvironment ?? readEnvironmentFromLoginShell)(shell, [
          ...CODEX_PROCESS_SHELL_ENV_NAMES,
          providerEnvKey,
        ]);

        if (shellEnvironment.PATH) {
          effectiveEnv.PATH = shellEnvironment.PATH;
        }
        if (!effectiveEnv.SSH_AUTH_SOCK && shellEnvironment.SSH_AUTH_SOCK) {
          effectiveEnv.SSH_AUTH_SOCK = shellEnvironment.SSH_AUTH_SOCK;
        }
        if (shellEnvironment[providerEnvKey]) {
          effectiveEnv[providerEnvKey] = shellEnvironment[providerEnvKey];
        }
      }
    } catch {
      // Keep inherited environment if shell lookup fails.
    }
  }

  return effectiveEnv;
}
