export const SENSITIVE_ACTION_CATEGORIES = [
  "protected-branch-publish",
  "ci-workflow-change",
  "dependency-change",
  "credential-access",
  "network-egress",
  "external-mcp-action",
] as const;

export type SensitiveActionCategory = (typeof SENSITIVE_ACTION_CATEGORIES)[number];
export type SensitiveActionAssessment = {
  readonly categories: ReadonlyArray<SensitiveActionCategory>;
  readonly requiresFreshApproval: boolean;
  readonly allowSessionApproval: false;
};

export interface SensitiveActionInput {
  readonly toolName?: string;
  readonly toolInput?: Readonly<Record<string, unknown>>;
}

const DEPENDENCY_FILE =
  /(?:^|\/)(?:package\.json|(?:bun|pnpm|yarn|package-lock)\.lockb?|go\.(?:mod|sum)|cargo\.(?:toml|lock)|pyproject\.toml)$/iu;
const CREDENTIAL_PATH =
  /(?:^|\/)(?:\.env(?:\.[^/]*)?|\.ssh|\.aws|\.kube\/config|\.npmrc|credentials?|secrets?)(?:\/|$)/iu;

export function classifySensitiveAction(input: SensitiveActionInput): SensitiveActionAssessment {
  const toolName = input.toolName?.trim() ?? "";
  const values = flattenStrings(input.toolInput);
  const combined = values.join("\n");
  const categories = new Set<SensitiveActionCategory>();
  if (/^mcp__/iu.test(toolName) || /(?:^|[._:-])mcp(?:[._:-]|$)/iu.test(toolName)) {
    categories.add("external-mcp-action");
  }
  if (/^(?:fetch|web[._:-]?(?:fetch|search))$/iu.test(toolName)) categories.add("network-egress");
  if (
    /(?:^|[\s;&|()])(?:[^\s;&|()]*\/)?git\b(?=[^;&|\r\n]{0,512}\b(?:push|send-pack)\b)/iu.test(
      combined,
    ) ||
    /\b(?:gh\s+(?:pr\s+merge|workflow\s+run|release\s+create)|glab\s+mr\s+merge)\b/iu.test(combined)
  ) {
    categories.add("protected-branch-publish");
  }
  if (
    /(?:^|[\s;&|()])(?:curl|wget|ssh|scp|sftp|nc|ncat|telnet)(?=[\s;&|()]|$)|\b(?:https?|ssh):\/\//iu.test(
      combined,
    )
  ) {
    categories.add("network-egress");
  }
  if (
    /\b(?:npm|pnpm|yarn|bun|go|cargo|pip3?|uv|poetry|bundle|gem|composer)\b(?=[^;&|\r\n]{0,512}\b(?:add|install|remove|update|upgrade|require|get|sync)\b)/iu.test(
      combined,
    ) ||
    values.some((value) => DEPENDENCY_FILE.test(value.replaceAll("\\", "/")))
  ) {
    categories.add("dependency-change");
  }
  if (
    values.some((value) => CREDENTIAL_PATH.test(value.replaceAll("\\", "/"))) ||
    /\b[A-Z][A-Z0-9_]{1,127}(?:_API_KEY|_AUTH_TOKEN|_CLIENT_SECRET|_PASSWORD|_PRIVATE_KEY|_TOKEN)\b/u.test(
      combined,
    )
  ) {
    categories.add("credential-access");
  }
  if (
    values.some((value) =>
      /(?:^|\/)(?:\.github\/workflows|\.gitlab-ci\.ya?ml|jenkinsfile|azure-pipelines\.ya?ml)(?:\/|$)/iu.test(
        value.replaceAll("\\", "/"),
      ),
    )
  ) {
    categories.add("ci-workflow-change");
  }
  const sorted = [...categories].sort();
  return {
    categories: sorted,
    requiresFreshApproval: sorted.length > 0,
    allowSessionApproval: false,
  };
}

function flattenStrings(value: unknown, depth = 0): string[] {
  if (depth > 4) return [];
  if (typeof value === "string") return [value];
  if (Array.isArray(value))
    return value.slice(0, 100).flatMap((item) => flattenStrings(item, depth + 1));
  if (value && typeof value === "object")
    return Object.values(value)
      .slice(0, 100)
      .flatMap((item) => flattenStrings(item, depth + 1));
  return [];
}
