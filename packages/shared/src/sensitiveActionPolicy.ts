import { type SensitiveActionAssessment, type SensitiveActionCategory } from "@synara/contracts";

export {
  SENSITIVE_ACTION_CATEGORIES,
  type SensitiveActionAssessment,
  type SensitiveActionCategory,
} from "@synara/contracts";

export interface SensitiveActionInput {
  readonly toolName?: string;
  readonly toolInput?: Readonly<Record<string, unknown>>;
  readonly command?: string;
  readonly paths?: ReadonlyArray<string>;
  readonly networkHost?: string;
}

interface SensitiveActionPatternRule {
  readonly category: SensitiveActionCategory;
  readonly patterns: ReadonlyArray<RegExp>;
}

const CREDENTIAL_ENV_PATTERN =
  /\b[A-Z][A-Z0-9_]{1,127}(?:_ACCESS_KEY(?:_ID)?|_ACCESS_TOKEN|_API_KEY|_ASKPASS|_AUTH_CONFIG|_AUTH_SOCK|_AUTH_TOKEN|_CLIENT_SECRET|_CREDENTIALS?|_PASSWORD|_PRIVATE_KEY|_SECRET|_SSH(?:_COMMAND)?|_TOKEN(?:_FILE)?|_URI)\b/u;

export const SENSITIVE_ACTION_CONTENT_TOOL_PATTERN =
  /^(?:apply_patch|create[_-]?file|edit|multi[_-]?edit|multi[_-]?replace[_-]?file[_-]?content|notebook[_-]?edit|replace[_-]?file[_-]?content|write|write[_-]?to[_-]?file)$/iu;

export const SENSITIVE_ACTION_CONTENT_INPUT_KEYS = [
  "codecontent",
  "code_content",
  "content",
  "newsource",
  "new_source",
  "newstring",
  "new_string",
  "replacement",
  "replacementcontent",
  "replacement_content",
] as const;

/**
 * Frozen pattern data is exported so Host-owned out-of-process guards can use
 * the exact same classifier without loading mutable workspace code.
 */
export const SENSITIVE_ACTION_POLICY_RULES = {
  toolName: [
    {
      category: "external-mcp-action",
      patterns: [/^mcp__/iu, /(?:^|[._:\s-])mcp(?:[._:\s-]|$)/iu],
    },
    {
      category: "network-egress",
      patterns: [/^(?:fetch|web[._:-]?(?:fetch|search))$/iu],
    },
  ],
  command: [
    {
      category: "protected-branch-publish",
      patterns: [
        /(?:^|[\s;&|()])(?:[^\s;&|()]*\/)?git\b(?=[^;&|\r\n]{0,512}\b(?:push|send-pack)\b)/iu,
        /\b(?:gh\s+(?:pr\s+merge|workflow\s+run|release\s+create)|glab\s+mr\s+merge)\b/iu,
      ],
    },
    {
      category: "network-egress",
      patterns: [
        /(?:^|[\/\s"';&|()])synara-agentd\s+--verify-kubernetes-network-boundary(?=[\s"';&|()]|$)/iu,
        /(?:^|[\s;&|()])(?:[^\s"';&|()]*\/)?(?:curl|wget|ssh|scp|sftp|nc|ncat|telnet|invoke-webrequest)(?=[\s;&|()]|$)/iu,
        /\b(?:https?|ssh):\/\//iu,
        /(?:^|[\s;&|()])(?:[^\s;&|()]*\/)?git\b(?=[^;&|\r\n]{0,512}\b(?:clone|fetch|pull|ls-remote|submodule\s+(?:add|update))\b)/iu,
        /\b(?:gh\b(?=[^;&|\r\n]{0,256}\bapi\b)|(?:docker|podman)\b(?=[^;&|\r\n]{0,256}\bpull\b))/iu,
      ],
    },
    {
      category: "credential-access",
      patterns: [
        /(?:^|[\/\s"';&|()])synara-agentd\s+--verify-provider-credential-scope(?=[\s"';&|()]|$)/iu,
        /\b(?:(?:gh|git|security|gcloud|aws|az|kubectl)\b(?=[^;&|\r\n]{0,256}\b(?:credential|secrets?|token|password|keyvault)\b)|aws\s+configure\s+get|kubectl\s+config\s+view\s+--raw|op\s+read|pass\s+show|sops\s+(?:--decrypt|-d)|vault\s+(?:kv\s+get|read))\b/iu,
        /(?:^|[\s"'=])(?:[^\s"']*\/)?(?:\.env(?:\.[^\s/"']*)?|\.ssh\/|\.aws\/|\.config\/gcloud\/|\.kube\/config\b|\.docker\/config\.json\b|\.npmrc\b|\.pypirc\b|\.netrc\b|[^\s/"']*credentials?(?:\.[^\s/"']*)?|[^\s/"']*secrets?(?:\.[^\s/"']*)?)/iu,
        CREDENTIAL_ENV_PATTERN,
        /(?:^|[;&|]\s*)(?:(?:[^\s;&|]*\/)?printenv\b|(?:[^\s;&|]*\/)?env\s*(?:$|[|>])|set\s*(?:$|[|>])|export\s+-p\b)|\/proc\/(?:self|\d+)\/environ\b|\bget-childitem\s+env:/iu,
      ],
    },
    {
      category: "dependency-change",
      patterns: [
        /\b(?:npm|pnpm|yarn|bun|go|cargo|pip3?|python(?:3(?:\.\d+)?)?|uv|poetry|bundle|gem|composer|dotnet|dart|flutter|mix)\b(?=[^;&|\r\n]{0,512}\b(?:add|install|remove|uninstall|update|upgrade|require|get|sync|deps\.(?:get|update))\b)/iu,
      ],
    },
    {
      category: "ci-workflow-change",
      patterns: [/\.(?:github\/workflows|gitlab-ci)|jenkinsfile|azure-pipelines/iu],
    },
  ],
  path: [
    {
      category: "ci-workflow-change",
      patterns: [
        /(^|\/)(\.github\/workflows|\.gitlab-ci\.ya?ml$|\.gitlab\/ci(?:\/|$)|\.circleci|\.buildkite(?:\/|$)|\.drone\.ya?ml$|\.woodpecker\.ya?ml$|bitbucket-pipelines\.ya?ml$|jenkinsfile$|azure-pipelines\.ya?ml$)/iu,
      ],
    },
    {
      category: "dependency-change",
      patterns: [
        /(^|\/)(package\.json|bun\.lockb?$|package-lock\.json$|pnpm-lock\.ya?ml$|yarn\.lock$|deno\.(jsonc?|lock)$|go\.(mod|sum)$|cargo\.(toml|lock)$|pom\.xml$|(?:build|settings)\.gradle(?:\.kts)?$|gradle\.lockfile$|gradle\/libs\.versions\.toml$|requirements[^/]*\.txt$|pyproject\.toml$|uv\.lock$|poetry\.lock$|pipfile(?:\.lock)?$|gemfile(?:\.lock)?$|composer\.(?:json|lock)$|package\.swift$|package\.resolved$|directory\.packages\.props$|packages\.lock\.json$|mix\.exs$|mix\.lock$|pubspec\.ya?ml$|pubspec\.lock$|flake\.nix$|flake\.lock$)/iu,
      ],
    },
    {
      category: "credential-access",
      patterns: [
        /(^|\/)(\.env(?:\.[^/]*)?$|\.ssh(?:\/|$)|\.aws(?:\/|$)|\.azure(?:\/|$)|\.config\/(?:gcloud|gh|glab-cli|op)(?:\/|$)|\.kube\/config$|\.docker\/config\.json$|\.npmrc$|\.pypirc$|\.netrc$|\.password-store(?:\/|$)|auth\.json$|credentials?(?:\.[^/]*)?$|secrets?(?:\.[^/]*)?$)/iu,
      ],
    },
    {
      category: "network-egress",
      patterns: [/(^|\/)(networkpolicy[^/]*\.ya?ml$|egress[^/]*\.ya?ml$|proxy[^/]*\.ya?ml$)/iu],
    },
  ],
  content: [
    {
      category: "network-egress",
      patterns: [/\b(?:https?|ssh|wss?):\/\//iu],
    },
    {
      category: "credential-access",
      patterns: [
        /(?:^|[\s"'`=])(?:[^\s"'`]*\/)?(?:\.env(?:\.[^\s/"'`]*)?|\.ssh\/|\.aws\/|\.azure\/|\.config\/(?:gcloud|gh|glab-cli|op)\/|\.kube\/config\b|\.docker\/config\.json\b|\.npmrc\b|\.pypirc\b|\.netrc\b|\.password-store\/|auth\.json\b|[^\s/"'`]*credentials?(?:\.[^\s/"'`]*)?|[^\s/"'`]*secrets?(?:\.[^\s/"'`]*)?)/iu,
        CREDENTIAL_ENV_PATTERN,
      ],
    },
  ],
} as const satisfies {
  readonly toolName: ReadonlyArray<SensitiveActionPatternRule>;
  readonly command: ReadonlyArray<SensitiveActionPatternRule>;
  readonly path: ReadonlyArray<SensitiveActionPatternRule>;
  readonly content: ReadonlyArray<SensitiveActionPatternRule>;
};

function stringsFromToolInput(
  input: Readonly<Record<string, unknown>> | undefined,
  toolName: string,
): {
  readonly commands: ReadonlyArray<string>;
  readonly paths: ReadonlyArray<string>;
  readonly hosts: ReadonlyArray<string>;
  readonly contents: ReadonlyArray<string>;
} {
  if (!input) return { commands: [], paths: [], hosts: [], contents: [] };
  const commands: string[] = [];
  const paths: string[] = [];
  const hosts: string[] = [];
  const contents: string[] = [];
  const collectContent = SENSITIVE_ACTION_CONTENT_TOOL_PATTERN.test(toolName);
  const contentKeys = new Set<string>(SENSITIVE_ACTION_CONTENT_INPUT_KEYS);
  const visit = (value: unknown, key: string, depth: number): void => {
    if (depth > 4) return;
    if (typeof value === "string") {
      const normalizedKey = key.toLowerCase();
      if (
        normalizedKey === "command" ||
        normalizedKey === "cmd" ||
        normalizedKey === "script" ||
        normalizedKey.endsWith("command")
      ) {
        commands.push(value);
      }
      if (
        normalizedKey === "path" ||
        normalizedKey === "paths" ||
        normalizedKey === "files" ||
        normalizedKey.endsWith("_path") ||
        normalizedKey.endsWith("path") ||
        normalizedKey === "file"
      ) {
        paths.push(value);
      }
      if (
        normalizedKey === "host" ||
        normalizedKey === "hostname" ||
        normalizedKey === "endpoint" ||
        normalizedKey.endsWith("endpoint") ||
        normalizedKey === "url" ||
        normalizedKey === "uri" ||
        normalizedKey.endsWith("url") ||
        normalizedKey.endsWith("uri")
      ) {
        hosts.push(value);
      }
      if (collectContent && contentKeys.has(normalizedKey)) contents.push(value);
      return;
    }
    if (Array.isArray(value)) {
      for (const item of value.slice(0, 100)) visit(item, key, depth + 1);
      return;
    }
    if (value && typeof value === "object") {
      for (const [childKey, child] of Object.entries(value).slice(0, 100)) {
        visit(child, childKey, depth + 1);
      }
    }
  };
  visit(input, "", 0);
  return { commands, paths, hosts, contents };
}

export function classifySensitiveAction(input: SensitiveActionInput): SensitiveActionAssessment {
  const toolName = input.toolName?.trim() ?? "";
  const extracted = stringsFromToolInput(input.toolInput, toolName);
  const commands = [input.command, ...extracted.commands].filter(
    (value): value is string => typeof value === "string" && value.trim().length > 0,
  );
  const paths = [
    ...(input.paths ?? []),
    ...extracted.paths,
    ...pathsFromApplyPatchCommand(input.toolName, input.toolInput),
    ...pathsFromFileChangeItem(input.toolName, input.toolInput),
  ];
  const hosts = [input.networkHost, ...extracted.hosts].filter(
    (value): value is string => typeof value === "string" && value.trim().length > 0,
  );
  const categories = new Set<SensitiveActionCategory>();
  addMatchingCategories(categories, toolName, SENSITIVE_ACTION_POLICY_RULES.toolName);
  if (hosts.length > 0) categories.add("network-egress");
  for (const command of commands) {
    addMatchingCategories(categories, command, SENSITIVE_ACTION_POLICY_RULES.command);
  }
  for (const path of paths) {
    const normalized = path.replaceAll("\\", "/").toLowerCase();
    addMatchingCategories(categories, normalized, SENSITIVE_ACTION_POLICY_RULES.path);
  }
  for (const content of extracted.contents) {
    addMatchingCategories(categories, content, SENSITIVE_ACTION_POLICY_RULES.content);
  }
  return {
    categories: [...categories].sort(),
    requiresFreshApproval: categories.size > 0,
    allowSessionApproval: false,
  };
}

export function mergeSensitiveActionAssessments(
  ...assessments: ReadonlyArray<SensitiveActionAssessment | undefined>
): SensitiveActionAssessment {
  const categories = new Set<SensitiveActionCategory>();
  for (const assessment of assessments) {
    for (const category of assessment?.categories ?? []) categories.add(category);
  }
  return {
    categories: [...categories].sort(),
    requiresFreshApproval: categories.size > 0,
    allowSessionApproval: false,
  };
}

function pathsFromApplyPatchCommand(
  toolName: string | undefined,
  toolInput: Readonly<Record<string, unknown>> | undefined,
): ReadonlyArray<string> {
  if (!/^(?:apply_patch|edit|write)$/iu.test(toolName?.trim() ?? "")) return [];
  const command = toolInput?.command;
  if (typeof command !== "string") return [];

  const paths: string[] = [];
  for (const pattern of [
    /^\*\*\* (?:Add|Update|Delete) File:\s*(.+)$/gmu,
    /^\*\*\* Move to:\s*(.+)$/gmu,
    /^(?:---|\+\+\+)\s+(?:[ab]\/)?([^\t\r\n]+)(?:\t.*)?$/gmu,
  ]) {
    for (const match of command.matchAll(pattern)) {
      const path = match[1]?.trim();
      if (path && path !== "/dev/null") paths.push(path);
    }
  }
  return paths;
}

function pathsFromFileChangeItem(
  toolName: string | undefined,
  toolInput: Readonly<Record<string, unknown>> | undefined,
): ReadonlyArray<string> {
  if (!/^(?:apply_patch|edit|write)$/iu.test(toolName?.trim() ?? "")) return [];
  return readCodexFileChangePaths(toolInput) ?? [];
}

export function readCodexFileChangePaths(
  toolInput: Readonly<Record<string, unknown>> | undefined,
): ReadonlyArray<string> | undefined {
  const changes = toolInput?.changes;
  if (!Array.isArray(changes) || changes.length === 0) return undefined;

  const paths: string[] = [];
  for (const change of changes) {
    if (!change || typeof change !== "object" || Array.isArray(change)) return undefined;
    const record = change as Record<string, unknown>;
    const path = safeFileChangePath(record.path);
    if (!path) return undefined;
    paths.push(path);
    const kind =
      record.kind && typeof record.kind === "object" && !Array.isArray(record.kind)
        ? (record.kind as Record<string, unknown>)
        : undefined;
    for (const container of [record, kind]) {
      if (!container) continue;
      for (const key of ["move_path", "movePath"] as const) {
        const movePathValue = container[key];
        if (movePathValue === undefined || movePathValue === null) continue;
        const movePath = safeFileChangePath(movePathValue);
        if (!movePath) return undefined;
        paths.push(movePath);
      }
    }
  }
  return paths;
}

function safeFileChangePath(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const path = value.trim();
  return path && path.length <= 4_096 && !/[\r\n\0]/u.test(path) ? path : undefined;
}

function addMatchingCategories(
  categories: Set<SensitiveActionCategory>,
  value: string,
  rules: ReadonlyArray<SensitiveActionPatternRule>,
): void {
  for (const rule of rules) {
    if (rule.patterns.some((pattern) => pattern.test(value))) categories.add(rule.category);
  }
}
