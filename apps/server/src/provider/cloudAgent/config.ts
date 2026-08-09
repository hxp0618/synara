import { isAbsolute, resolve } from "node:path";

export type CodexBackend = "native" | "cloud-agent";

export interface CloudAgentBackendConfig {
  readonly backend: CodexBackend;
  readonly runtimePath?: string;
  readonly runtimeSha256?: string;
}

const SHA256_PATTERN = /^[0-9a-f]{64}$/u;

export function readCloudAgentBackendConfig(
  environment: Readonly<Record<string, string | undefined>> = process.env,
): CloudAgentBackendConfig {
  const configuredBackend = environment.SYNARA_CODEX_BACKEND?.trim().toLowerCase();
  if (
    configuredBackend !== undefined &&
    configuredBackend !== "" &&
    configuredBackend !== "native" &&
    configuredBackend !== "cloud-agent"
  ) {
    throw new Error("SYNARA_CODEX_BACKEND must be 'native' or 'cloud-agent'.");
  }
  const backend = configuredBackend === "cloud-agent" ? "cloud-agent" : "native";
  const configuredPath = environment.SYNARA_CLOUD_AGENT_RUNTIME_PATH?.trim();
  const configuredDigest = environment.SYNARA_CLOUD_AGENT_RUNTIME_SHA256?.trim().toLowerCase();
  if (configuredDigest && !SHA256_PATTERN.test(configuredDigest)) {
    throw new Error("SYNARA_CLOUD_AGENT_RUNTIME_SHA256 must be a lowercase SHA-256 digest.");
  }
  if (configuredPath && !isAbsolute(configuredPath)) {
    throw new Error("SYNARA_CLOUD_AGENT_RUNTIME_PATH must be absolute.");
  }
  if (configuredDigest && !configuredPath) {
    throw new Error("SYNARA_CLOUD_AGENT_RUNTIME_SHA256 requires SYNARA_CLOUD_AGENT_RUNTIME_PATH.");
  }
  return {
    backend,
    ...(configuredPath ? { runtimePath: resolve(configuredPath) } : {}),
    ...(configuredDigest ? { runtimeSha256: configuredDigest } : {}),
  };
}
