import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import {
  EXECUTION_TARGET_CAPABILITIES_PLACEHOLDER,
  ExecutionTargetSettingsSection,
  ExecutionTargetPolicyDisclosure,
} from "./ExecutionTargetSettingsSection";
import type {
  ControlPlaneExecutionTarget,
  ControlPlaneWorkerManifest,
  ControlPlaneWorkerProviderManifest,
} from "~/lib/controlPlaneClient";

const observedCapabilities = {
  discovery: "native",
  "start-session": "native",
  "resume-session": "emulated",
  "send-turn": "native",
  "worker-migration": "unsupported",
} as ControlPlaneWorkerProviderManifest["capabilities"];

function executionTarget(
  overrides: Partial<ControlPlaneExecutionTarget> = {},
): ControlPlaneExecutionTarget {
  return {
    id: "target-1",
    tenantId: "tenant-1",
    organizationId: null,
    kind: "docker",
    name: "Docker workers",
    status: "active",
    capabilities: {},
    createdAt: "2026-07-14T00:00:00Z",
    updatedAt: "2026-07-14T00:00:00Z",
    ...overrides,
  };
}

function workerManifest(input: {
  manifestId: string;
  online: number;
  draining?: number;
  offline?: number;
  lastHeartbeatAt: string;
  version: string;
}): ControlPlaneWorkerManifest {
  return {
    executionTargetId: "target-1",
    manifestId: input.manifestId,
    workerStatusCounts: {
      online: input.online,
      draining: input.draining ?? 0,
      offline: input.offline ?? 0,
    },
    lastHeartbeatAt: input.lastHeartbeatAt,
    workerBuild: {
      version: input.version,
      gitSha: "abc123",
      imageDigest: `sha256:${input.manifestId}`,
      operatingSystem: "linux",
      architecture: "arm64",
    },
    workerProtocol: { minimum: 2, maximum: 2 },
    runtimeEvent: { minimum: 2, maximum: 3 },
    providers: [
      {
        provider: "codex",
        supportTier: "experimental",
        compatibilityStatus: "compatible",
        runtime: {
          kind: "cli",
          name: "codex-cli",
          version: "0.144.1",
          available: true,
          versionSource: "probe",
          compatibleRange: {
            minimumInclusive: "0.144.1",
            maximumExclusive: "0.145.0",
          },
          compatible: true,
        },
        releasePolicy: { requiresExplicitEnablement: true, enabled: true },
        capabilities: observedCapabilities,
      },
    ],
  };
}

function renderExecutionTargetSection(input: {
  target: ControlPlaneExecutionTarget;
  manifests?: ReadonlyArray<ControlPlaneWorkerManifest>;
  canManage?: boolean;
}): string {
  return renderToStaticMarkup(
    <QueryClientProvider client={new QueryClient()}>
      <ExecutionTargetSettingsSection
        canManage={input.canManage ?? false}
        error={null}
        isLoading={false}
        onCreated={() => undefined}
        onProviderPolicyUpdated={() => undefined}
        onUpdated={() => undefined}
        organizations={[]}
        requireOrganizationScope={false}
        targets={[input.target]}
        tenantId="tenant-1"
        workerManifests={input.manifests ?? []}
        workerManifestsError={null}
        workerManifestsLoading={false}
      />
    </QueryClientProvider>,
  );
}

describe("Execution Target Provider Policy", () => {
  it("documents explicit Experimental enablement in the capabilities example", () => {
    expect(JSON.parse(EXECUTION_TARGET_CAPABILITIES_PLACEHOLDER)).toEqual({
      workspaceModes: ["local", "worktree"],
      providerPolicy: { experimentalProviders: ["codex", "claudeAgent"] },
    });
  });

  it("renders persisted policy and a neutral Kubernetes not-observed state", () => {
    const markup = renderToStaticMarkup(
      <ExecutionTargetPolicyDisclosure
        target={executionTarget({
          organizationId: "organization-1",
          kind: "kubernetes",
          name: "Worker pool",
          capabilities: {
            providerPolicy: { experimentalProviders: ["codex", "claudeAgent"] },
          },
        })}
      />,
    );

    expect(markup).toContain("Observed manifest");
    expect(markup).toContain("codex, claudeAgent");
    expect(markup).toContain("Not observed yet");
    expect(markup).toContain("does not make the Kubernetes target unavailable");
    expect(markup).toContain('aria-expanded="false"');
  });

  it("renders observed build, runtime, release policy, and capability evidence", () => {
    const markup = renderToStaticMarkup(
      <ExecutionTargetPolicyDisclosure
        manifests={[
          workerManifest({
            manifestId: "manifest-1",
            online: 2,
            draining: 1,
            lastHeartbeatAt: "2026-07-14T08:00:00Z",
            version: "0.5.2",
          }),
        ]}
        target={executionTarget()}
      />,
    );

    expect(markup).toContain("manifest-1");
    expect(markup).toContain("2 online · 1 draining · 0 offline");
    expect(markup).toContain("linux/arm64");
    expect(markup).toContain("codex-cli");
    expect(markup).toContain("Explicitly enabled");
    expect(markup).toContain("resume-session");
    expect(markup).toContain("worker-migration");
  });

  it("shows every manifest variant in a stable online-first order with an aggregate summary", () => {
    const oldOfflineVariant = workerManifest({
      manifestId: "manifest-old",
      online: 1,
      offline: 3,
      lastHeartbeatAt: "2026-07-14T07:00:00Z",
      version: "0.5.1",
    });
    const currentOnlineVariant = workerManifest({
      manifestId: "manifest-current",
      online: 2,
      lastHeartbeatAt: "2026-07-14T08:00:00Z",
      version: "0.5.2",
    });

    const markup = renderExecutionTargetSection({
      target: executionTarget(),
      manifests: [oldOfflineVariant, currentOnlineVariant],
    });

    expect(markup).toContain("2 variants · 3 online");
    expect(markup).toContain("manifest-old");
    expect(markup).toContain("manifest-current");
    expect(markup.indexOf("manifest-current")).toBeLessThan(markup.indexOf("manifest-old"));
  });

  it("does not describe an unprojected shared target as not observed", () => {
    const markup = renderToStaticMarkup(
      <ExecutionTargetPolicyDisclosure target={executionTarget({ tenantId: null })} />,
    );

    expect(markup).toContain("Shared target observation is not available");
    expect(markup).not.toContain("Not observed yet");
  });

  it("offers an explicit Provider Policy update path for tenant-owned targets", () => {
    const markup = renderExecutionTargetSection({
      canManage: true,
      target: executionTarget({
        capabilities: {
          providerPolicy: { experimentalProviders: ["codex"] },
        },
      }),
    });

    expect(markup).toContain("Disable Codex");
    expect(markup).toContain("Enable Claude");
    expect(markup).toContain("re-registers with the new policy");
  });

});
