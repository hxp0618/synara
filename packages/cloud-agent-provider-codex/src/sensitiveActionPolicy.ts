export type SensitiveActionCategory =
  | "protected-branch-publish"
  | "ci-workflow-change"
  | "dependency-change"
  | "credential-access"
  | "network-egress"
  | "external-mcp-action";
export type SensitiveActionAssessment = {
  readonly categories: ReadonlyArray<SensitiveActionCategory>;
  readonly requiresFreshApproval: boolean;
  readonly allowSessionApproval: false;
};
export const SENSITIVE_ACTION_POLICY_RULES = {
  toolName: [{ category: "external-mcp-action" as const, patterns: [/^mcp__/iu] }],
  command: [
    {
      category: "protected-branch-publish" as const,
      patterns: [
        /(?:^|[\s;&|()])(?:[^\s;&|()]*\/)?git\b(?=[^;&|\r\n]{0,512}\b(?:push|send-pack)\b)/iu,
      ],
    },
    {
      category: "network-egress" as const,
      patterns: [
        /\b(?:https?|ssh):\/\//iu,
        /(?:^|[\s;&|()])(?:curl|wget|ssh|scp|sftp|nc|ncat|telnet)(?=[\s;&|()]|$)/iu,
      ],
    },
    {
      category: "credential-access" as const,
      patterns: [
        /\b[A-Z][A-Z0-9_]{1,127}(?:_API_KEY|_AUTH_TOKEN|_CLIENT_SECRET|_PASSWORD|_PRIVATE_KEY|_TOKEN)\b/u,
        /\b(?:printenv|export\s+-p)\b/iu,
      ],
    },
    {
      category: "dependency-change" as const,
      patterns: [
        /\b(?:npm|pnpm|yarn|bun|go|cargo|pip3?)\b(?=[^;&|\r\n]{0,512}\b(?:add|install|remove|update|upgrade|get)\b)/iu,
      ],
    },
  ],
  path: [
    {
      category: "dependency-change" as const,
      patterns: [
        /(?:^|\/)(?:package\.json|bun\.lockb?|package-lock\.json|pnpm-lock\.ya?ml|yarn\.lock|go\.(?:mod|sum)|cargo\.(?:toml|lock))(?:$|\/)/iu,
      ],
    },
    {
      category: "ci-workflow-change" as const,
      patterns: [
        /(?:^|\/)(?:\.github\/workflows|\.gitlab-ci\.ya?ml|jenkinsfile|azure-pipelines\.ya?ml)(?:$|\/)/iu,
      ],
    },
    {
      category: "credential-access" as const,
      patterns: [
        /(?:^|\/)(?:\.env|\.ssh|\.aws|\.kube\/config|\.npmrc|credentials?|secrets?)(?:$|\/)/iu,
      ],
    },
  ],
  content: [
    { category: "network-egress" as const, patterns: [/\b(?:https?|ssh|wss?):\/\//iu] },
    {
      category: "credential-access" as const,
      patterns: [
        /\b[A-Z][A-Z0-9_]{1,127}(?:_API_KEY|_AUTH_TOKEN|_CLIENT_SECRET|_PASSWORD|_PRIVATE_KEY|_TOKEN)\b/u,
      ],
    },
  ],
} as const;

export function classifySensitiveAction(input: {
  readonly toolName?: string;
  readonly toolInput?: Readonly<Record<string, unknown>>;
  readonly paths?: ReadonlyArray<string>;
  readonly command?: string;
  readonly networkHost?: string;
}): SensitiveActionAssessment {
  const toolName = input.toolName?.trim() ?? "";
  const values = [
    input.command,
    input.networkHost,
    ...(input.paths ?? []),
    ...flattenStrings(input.toolInput),
  ].filter((value): value is string => typeof value === "string");
  const combined = values.join("\n");
  const categories = new Set<SensitiveActionCategory>();
  if (input.networkHost) categories.add("network-egress");
  if (
    /(?:^|[\s/])(?:package\.json|bun\.lockb?|package-lock\.json|pnpm-lock\.ya?ml|yarn\.lock)(?:$|[\s/])/imu.test(
      combined,
    )
  )
    categories.add("dependency-change");
  for (const rule of SENSITIVE_ACTION_POLICY_RULES.toolName)
    if (rule.patterns.some((p) => p.test(toolName))) categories.add(rule.category);
  for (const rule of SENSITIVE_ACTION_POLICY_RULES.command)
    if (rule.patterns.some((p) => p.test(combined))) categories.add(rule.category);
  for (const value of values) {
    const normalized = value.replaceAll("\\", "/");
    for (const rule of SENSITIVE_ACTION_POLICY_RULES.path)
      if (rule.patterns.some((p) => p.test(normalized))) categories.add(rule.category);
    for (const rule of SENSITIVE_ACTION_POLICY_RULES.content)
      if (rule.patterns.some((p) => p.test(value))) categories.add(rule.category);
  }
  const sorted = [...categories].sort();
  return {
    categories: sorted,
    requiresFreshApproval: sorted.length > 0,
    allowSessionApproval: false,
  };
}
export function mergeSensitiveActionAssessments(
  ...values: ReadonlyArray<SensitiveActionAssessment | undefined>
): SensitiveActionAssessment {
  const categories = [...new Set(values.flatMap((value) => value?.categories ?? []))].sort();
  return { categories, requiresFreshApproval: categories.length > 0, allowSessionApproval: false };
}
export function readCodexFileChangePaths(
  input: Readonly<Record<string, unknown>> | undefined,
): ReadonlyArray<string> | undefined {
  if (!Array.isArray(input?.changes) || input.changes.length === 0) return undefined;
  const paths = input.changes.flatMap((value) =>
    value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    typeof (value as Record<string, unknown>).path === "string"
      ? [(value as Record<string, unknown>).path as string]
      : [],
  );
  return paths.length === input.changes.length ? paths : undefined;
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
