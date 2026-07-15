import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { WorkerReleaseControls, workerReleasesQueryKey } from "./WorkerReleaseControls";
import type {
  ControlPlaneExecutionTarget,
  ControlPlaneWorkerManifest,
  ControlPlaneWorkerReleaseOverview,
} from "~/lib/controlPlaneClient";

const target: ControlPlaneExecutionTarget = {
  id: "target-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  kind: "docker",
  name: "Docker workers",
  status: "active",
  capabilities: {},
  createdAt: "2026-07-15T00:00:00Z",
  updatedAt: "2026-07-15T00:00:00Z",
};

const manifests: ReadonlyArray<ControlPlaneWorkerManifest> = [
  {
    executionTargetId: target.id,
    manifestId: "manifest-1",
    workerStatusCounts: { online: 2, draining: 0, offline: 0 },
    lastHeartbeatAt: "2026-07-15T00:00:00Z",
    workerBuild: {
      version: "0.6.0",
      imageDigest: `sha256:${"a".repeat(64)}`,
      operatingSystem: "linux",
      architecture: "amd64",
    },
    workerProtocol: { minimum: 2, maximum: 2 },
    runtimeEvent: { minimum: 2, maximum: 2 },
    providers: [],
  },
  {
    executionTargetId: target.id,
    manifestId: "manifest-2",
    workerStatusCounts: { online: 1, draining: 0, offline: 0 },
    lastHeartbeatAt: "2026-07-15T01:00:00Z",
    workerBuild: {
      version: "0.7.0",
      imageDigest: `sha256:${"b".repeat(64)}`,
      operatingSystem: "linux",
      architecture: "amd64",
    },
    workerProtocol: { minimum: 2, maximum: 2 },
    runtimeEvent: { minimum: 2, maximum: 2 },
    providers: [],
  },
];

function renderReleaseControls(overview: ControlPlaneWorkerReleaseOverview): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(workerReleasesQueryKey("tenant-1", "target-1"), overview);
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <WorkerReleaseControls
        canManage
        enabled
        manifests={manifests}
        target={target}
        tenantId="tenant-1"
      />
    </QueryClientProvider>,
  );
}

describe("WorkerReleaseControls", () => {
  it("offers initial promotion and immutable revision registration", () => {
    const markup = renderReleaseControls({
      policy: null,
      revisions: [
        {
          id: "release-1",
          tenantId: "tenant-1",
          executionTargetId: "target-1",
          revision: 1,
          workerManifestId: "manifest-1",
          workerBuildVersion: "0.6.0",
          imageDigest: `sha256:${"a".repeat(64)}`,
          description: "Baseline",
          createdBy: "user-1",
          createdAt: "2026-07-15T00:00:00Z",
        },
      ],
      transitions: [],
    });

    expect(markup).toContain("Promote baseline");
    expect(markup).toContain("Register immutable revision");
    expect(markup).toContain("0.7.0 · manifest-2");
    expect(markup).not.toContain("0.6.0 · manifest-1");
  });

  it("shows canary promotion and abort controls while preserving the promoted baseline", () => {
    const markup = renderReleaseControls({
      policy: {
        tenantId: "tenant-1",
        executionTargetId: "target-1",
        policyVersion: 2,
        promotedRevisionId: "release-1",
        canaryRevisionId: "release-2",
        canaryPercent: 10,
        updatedBy: "user-1",
        updatedAt: "2026-07-15T01:00:00Z",
      },
      revisions: [
        {
          id: "release-2",
          tenantId: "tenant-1",
          executionTargetId: "target-1",
          revision: 2,
          workerManifestId: "manifest-2",
          workerBuildVersion: "0.7.0",
          imageDigest: `sha256:${"b".repeat(64)}`,
          description: "Candidate",
          createdBy: "user-1",
          createdAt: "2026-07-15T01:00:00Z",
        },
        {
          id: "release-1",
          tenantId: "tenant-1",
          executionTargetId: "target-1",
          revision: 1,
          workerManifestId: "manifest-1",
          workerBuildVersion: "0.6.0",
          imageDigest: `sha256:${"a".repeat(64)}`,
          description: "Baseline",
          createdBy: "user-1",
          createdAt: "2026-07-15T00:00:00Z",
        },
      ],
      transitions: [],
    });

    expect(markup).toContain("policy v2");
    expect(markup).toContain("canary 10%");
    expect(markup).toContain("promoted");
    expect(markup).toContain("Promote canary");
    expect(markup).toContain("Abort canary");
  });

  it("offers one immutable managed manifest observed on another target", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
    });
    queryClient.setQueryData(workerReleasesQueryKey("tenant-1", "target-1"), {
      policy: null,
      revisions: [],
      transitions: [],
    } satisfies ControlPlaneWorkerReleaseOverview);
    const externalManifest = {
      ...manifests[1]!,
      executionTargetId: "staging-target",
    };
    const markup = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <WorkerReleaseControls
          canManage
          enabled
          manifests={[externalManifest, externalManifest]}
          target={target}
          tenantId="tenant-1"
        />
      </QueryClientProvider>,
    );

    expect(markup).toContain("observed elsewhere");
    expect(markup.match(/<option/g)).toHaveLength(1);
  });

  it("renders the newest release transition with abort-canary semantics", () => {
    const markup = renderReleaseControls({
      policy: {
        tenantId: "tenant-1",
        executionTargetId: "target-1",
        policyVersion: 3,
        promotedRevisionId: "release-1",
        canaryPercent: 0,
        updatedBy: "user-1",
        updatedAt: "2026-07-15T02:00:00Z",
      },
      revisions: [],
      transitions: [
        {
          id: "transition-3",
          tenantId: "tenant-1",
          executionTargetId: "target-1",
          policyVersion: 3,
          action: "abort-canary",
          fromPromotedRevisionId: "release-1",
          fromCanaryRevisionId: "release-2",
          toPromotedRevisionId: "release-1",
          canaryPercent: 0,
          reason: "candidate health gate failed",
          actorId: "user-1",
          occurredAt: "2026-07-15T02:00:00Z",
        },
      ],
    });

    expect(markup).toContain("Recent policy changes");
    expect(markup).toContain("policy v3");
    expect(markup).toContain("canary aborted");
    expect(markup).toContain("candidate health gate failed");
  });
});
