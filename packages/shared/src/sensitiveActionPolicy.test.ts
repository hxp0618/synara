import { describe, expect, it } from "vitest";

import {
  classifySensitiveAction,
  mergeSensitiveActionAssessments,
  readCodexFileChangePaths,
} from "./sensitiveActionPolicy";

describe("sensitive action policy", () => {
  it("requires a fresh approval for protected publication and network egress", () => {
    expect(
      classifySensitiveAction({ command: "git push --force-with-lease origin main" }),
    ).toMatchObject({
      categories: ["protected-branch-publish"],
      requiresFreshApproval: true,
      allowSessionApproval: false,
    });
    expect(
      classifySensitiveAction({ toolName: "WebFetch", toolInput: { url: "https://x" } }).categories,
    ).toContain("network-egress");
  });

  it("recognizes CI, dependency, and credential paths in nested tool input", () => {
    const assessment = classifySensitiveAction({
      toolName: "MultiEdit",
      toolInput: {
        edits: [
          { file_path: ".github/workflows/release.yml" },
          { file_path: "package.json" },
          { file_path: "/home/user/.ssh/config" },
        ],
      },
    });
    expect(assessment.categories).toEqual([
      "ci-workflow-change",
      "credential-access",
      "dependency-change",
    ]);
  });

  it("does not classify ordinary source edits as sensitive", () => {
    expect(
      classifySensitiveAction({ toolName: "Edit", toolInput: { file_path: "src/parser.ts" } }),
    ).toEqual({ categories: [], requiresFreshApproval: false, allowSessionApproval: false });
    expect(classifySensitiveAction({ command: "env NODE_ENV=test npm run build" })).toEqual({
      categories: [],
      requiresFreshApproval: false,
      allowSessionApproval: false,
    });
  });

  it("classifies provider-native permission payloads and mutating package commands", () => {
    expect(
      classifySensitiveAction({
        toolName: "execute",
        command: "git -C /workspace push origin main",
        toolInput: { rawInput: { command: "bun add attacker-package" } },
      }).categories,
    ).toEqual(["dependency-change", "protected-branch-publish"]);

    expect(
      classifySensitiveAction({
        toolName: "edit",
        toolInput: {
          rawInput: { path: ".github/workflows/release.yml" },
          locations: [{ path: "/home/runner/.aws/credentials" }],
        },
      }).categories,
    ).toEqual(["ci-workflow-change", "credential-access"]);

    expect(classifySensitiveAction({ toolName: "fetch" }).categories).toEqual(["network-egress"]);
  });

  it("handles git global flags, absolute credential paths, and common dependency files", () => {
    expect(
      classifySensitiveAction({
        command: "git --no-pager -c core.fsmonitor=false -C /workspace push origin main",
      }).categories,
    ).toEqual(["protected-branch-publish"]);
    expect(
      classifySensitiveAction({
        command: "sed -n '1p' /home/runner/.aws/credentials",
      }).categories,
    ).toEqual(["credential-access"]);
    expect(
      classifySensitiveAction({
        toolName: "edit",
        toolInput: { files: ["build.gradle.kts", "poetry.lock", ".docker/config.json"] },
      }).categories,
    ).toEqual(["credential-access", "dependency-change"]);
  });

  it("cannot bypass command classification with global package or Git flags", () => {
    expect(
      classifySensitiveAction({
        command: "git -C /workspace -c protocol.version=2 fetch origin main",
      }).categories,
    ).toEqual(["network-egress"]);
    expect(
      classifySensitiveAction({
        command: "npm --prefix apps/web install attacker-package",
      }).categories,
    ).toEqual(["dependency-change"]);
    expect(
      classifySensitiveAction({
        command:
          "python -m pip --index-url https://packages.example/simple install attacker-package",
      }).categories,
    ).toEqual(["dependency-change", "network-egress"]);
  });

  it("classifies sensitive data introduced by mutating file tools", () => {
    expect(
      classifySensitiveAction({
        toolName: "Write",
        toolInput: {
          file_path: "src/config.ts",
          content: 'export const endpoint = "https://attacker.example/upload";',
        },
      }).categories,
    ).toEqual(["network-egress"]);
    expect(
      classifySensitiveAction({
        toolName: "Edit",
        toolInput: {
          file_path: "src/config.ts",
          old_string: "export const token = undefined;",
          new_string: "export const token = process.env.CUSTOM_API_KEY;",
        },
      }).categories,
    ).toEqual(["credential-access"]);
    expect(
      classifySensitiveAction({
        toolName: "Edit",
        toolInput: {
          file_path: "src/config.ts",
          old_string: 'export const retired = "https://old.example";',
          new_string: 'export const retired = "disabled";',
        },
      }).categories,
    ).toEqual([]);
    expect(
      classifySensitiveAction({
        toolName: "Write",
        toolInput: { file_path: "src/message.ts", content: 'export const message = "hello";' },
      }).categories,
    ).toEqual([]);
  });

  it("covers common CI, dependency, and credential ecosystems", () => {
    expect(
      classifySensitiveAction({
        toolName: "MultiEdit",
        toolInput: {
          edits: [
            { file_path: ".gitlab/ci/deploy.yml" },
            { file_path: ".buildkite/pipeline.yml" },
            { file_path: "composer.json" },
            { file_path: "Package.swift" },
            { file_path: "packages.lock.json" },
            { file_path: ".config/gh/hosts.yml" },
            { file_path: "auth.json" },
          ],
        },
      }).categories,
    ).toEqual(["ci-workflow-change", "credential-access", "dependency-change"]);
    for (const command of [
      "composer --working-dir app require attacker/package",
      "dotnet add src/App package Attacker.Package",
    ]) {
      expect(classifySensitiveAction({ command }).categories, command).toEqual([
        "dependency-change",
      ]);
    }
    for (const command of [
      "aws secretsmanager get-secret-value --secret-id production",
      "az keyvault secret show --vault-name production --name api-key",
      "gcloud secrets versions access latest --secret api-key",
      "kubectl get secret production -o json",
      "op read op://production/api-key/password",
    ]) {
      expect(classifySensitiveAction({ command }).categories, command).toEqual([
        "credential-access",
      ]);
    }
  });

  it("classifies explicit URLs, provider endpoint fields, and credential environment reads", () => {
    expect(
      classifySensitiveAction({ command: "python script.py https://attacker.example/upload" })
        .categories,
    ).toEqual(["network-egress"]);
    expect(
      classifySensitiveAction({ toolName: "custom", toolInput: { apiEndpoint: "https://api" } })
        .categories,
    ).toEqual(["network-egress"]);
    expect(
      classifySensitiveAction({ command: "printenv AWS_SECRET_ACCESS_KEY" }).categories,
    ).toEqual(["credential-access"]);
    for (const command of [
      'echo "$GITHUB_TOKEN"',
      'echo "$AWS_WEB_IDENTITY_TOKEN_FILE"',
      'echo "$AZURE_FEDERATED_TOKEN_FILE"',
      'echo "$DOCKER_AUTH_CONFIG"',
      'echo "$SSH_AUTH_SOCK"',
      'echo "$NODE_AUTH_TOKEN"',
      "node -e 'process.stdout.write(process.env.ANTHROPIC_API_KEY ?? \"\")'",
      "env",
      "tr '\\0' '\\n' </proc/self/environ",
      "powershell Get-ChildItem Env:",
    ]) {
      expect(classifySensitiveAction({ command }).categories, command).toEqual([
        "credential-access",
      ]);
    }
  });

  it("classifies the bounded Stage 5 ambient-credential probe without inventing egress", () => {
    const command = String.raw`node -e 'const f=require("node:fs"),e=["AWS_ACCESS_KEY_ID","GITHUB_TOKEN","NPM_TOKEN"];
const a=e.some(n=>Object.prototype.hasOwnProperty.call(process.env,n));
const b=[".aws/credentials",".ssh/id_ed25519"].some(p=>f.existsSync(p));
const u=/https?:\/\/[^\s/]+@/iu.test("");'`;

    expect(classifySensitiveAction({ command }).categories).toEqual(["credential-access"]);
    expect(classifySensitiveAction({ command: "/usr/bin/ssh build-host" }).categories).toEqual([
      "network-egress",
    ]);
    expect(
      classifySensitiveAction({
        command: "/usr/local/bin/synara-agentd --verify-provider-credential-scope",
      }).categories,
    ).toEqual(["credential-access"]);
    expect(
      classifySensitiveAction({
        command: "/usr/local/bin/synara-agentd --verify-kubernetes-network-boundary",
      }).categories,
    ).toEqual(["network-egress"]);
    expect(
      classifySensitiveAction({
        command: '/bin/bash -lc "/usr/local/bin/synara-agentd --verify-provider-credential-scope"',
      }).categories,
    ).toEqual(["credential-access"]);
    expect(
      classifySensitiveAction({
        command:
          '/bin/bash -lc "/usr/local/bin/synara-agentd --verify-kubernetes-network-boundary"',
      }).categories,
    ).toEqual(["network-egress"]);
  });

  it("classifies the safety-fused malicious-Issue denial probe canonically", () => {
    expect(
      classifySensitiveAction({
        command: "false && git push origin main && printenv GITHUB_TOKEN",
      }),
    ).toEqual({
      categories: ["credential-access", "protected-branch-publish"],
      requiresFreshApproval: true,
      allowSessionApproval: false,
    });
  });

  it("extracts sensitive paths from Codex apply_patch envelopes", () => {
    expect(
      classifySensitiveAction({
        toolName: "apply_patch",
        toolInput: {
          command: [
            "*** Begin Patch",
            "*** Update File: package.json",
            "*** Move to: .github/workflows/release.yml",
            "*** Add File: .env.production",
            "*** Update File: deploy/networkpolicy.yaml",
            "*** End Patch",
          ].join("\n"),
        },
      }).categories,
    ).toEqual(["ci-workflow-change", "credential-access", "dependency-change", "network-egress"]);
  });

  it("merges cached tool and native request assessments without session approval", () => {
    expect(
      mergeSensitiveActionAssessments(
        classifySensitiveAction({ paths: ["package.json"] }),
        classifySensitiveAction({ paths: [".github/workflows/release.yml"] }),
      ),
    ).toEqual({
      categories: ["ci-workflow-change", "dependency-change"],
      requiresFreshApproval: true,
      allowSessionApproval: false,
    });
  });

  it("classifies every path in a large native file-change item", () => {
    expect(
      classifySensitiveAction({
        toolName: "apply_patch",
        toolInput: {
          changes: [
            ...Array.from({ length: 100 }, (_, index) => ({ path: `src/file-${index}.ts` })),
            { path: "package.json" },
          ],
        },
      }).categories,
    ).toEqual(["dependency-change"]);
  });

  it("rejects malformed or empty native file-change path sets", () => {
    expect(readCodexFileChangePaths({ changes: [] })).toBeUndefined();
    expect(
      readCodexFileChangePaths({ changes: [{ path: "package.json\nignored" }] }),
    ).toBeUndefined();
    expect(readCodexFileChangePaths({ changes: [{ kind: { type: "update" } }] })).toBeUndefined();
    expect(
      classifySensitiveAction({
        toolName: "apply_patch",
        toolInput: {
          changes: [
            {
              path: "src/ordinary.ts",
              kind: { type: "update", move_path: ".github/workflows/release.yml" },
            },
          ],
        },
      }).categories,
    ).toEqual(["ci-workflow-change"]);
  });
});
