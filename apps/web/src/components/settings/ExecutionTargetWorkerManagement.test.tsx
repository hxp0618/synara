import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { ExecutionTargetWorkerManagement } from "./ExecutionTargetWorkerManagement";
import type { ControlPlaneExecutionTarget, ControlPlaneWorker } from "~/lib/controlPlaneClient";

const target: ControlPlaneExecutionTarget = {
  id: "target-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  kind: "kubernetes",
  name: "Production Kubernetes",
  status: "active",
  capabilities: {},
  createdAt: "2026-07-22T00:00:00Z",
  updatedAt: "2026-07-22T00:00:00Z",
};

function worker(overrides: Partial<ControlPlaneWorker> = {}): ControlPlaneWorker {
  return {
    id: "worker-1",
    incarnation: 7,
    instanceUid: "pod-uid-1",
    executionTargetId: target.id,
    targetKind: "kubernetes",
    clusterId: "stage3-prod",
    namespace: "synara-workers",
    podName: "synara-worker-1",
    version: "0.6.0",
    protocolVersion: 2,
    currentManifestId: "manifest-1",
    compatibilityStatus: "compatible",
    workerReleaseRevisionId: "release-1",
    workerReleaseChannel: "promoted",
    workerReleaseStatus: "active",
    leaseSupported: true,
    fencingSupported: true,
    status: "online",
    administrativeStatus: "active",
    registeredAt: "2026-07-22T00:00:00Z",
    lastHeartbeatAt: "2026-07-22T00:01:00Z",
    ...overrides,
  };
}

function renderWorkers(workers: ReadonlyArray<ControlPlaneWorker>, canManage = true): string {
  return renderToStaticMarkup(
    <QueryClientProvider client={new QueryClient()}>
      <ExecutionTargetWorkerManagement
        canManage={canManage}
        target={target}
        tenantId="tenant-1"
        workers={workers}
      />
    </QueryClientProvider>,
  );
}

describe("ExecutionTargetWorkerManagement", () => {
  it("renders exact Worker identity, protocol, release, and fencing evidence", () => {
    const markup = renderWorkers([worker()]);

    expect(markup).toContain("synara-worker-1");
    expect(markup).toContain("stage3-prod");
    expect(markup).toContain("manifest-1");
    expect(markup).toContain("0.6.0 · protocol 2");
    expect(markup).toContain("lease · fencing");
  });

  it("caps revocation reasons at the API boundary", () => {
    expect(renderWorkers([worker()])).toContain('maxLength="2000"');
  });

  it("does not offer duplicate revocation for an already revoked Worker", () => {
    const markup = renderWorkers([
      worker({
        administrativeStatus: "revoked",
        revokedAt: "2026-07-22T00:02:00Z",
        revocationReason: "Compromised identity",
      }),
    ]);

    expect(markup).toContain("Compromised identity");
    expect(markup).not.toContain("Open revoke controls");
  });
});
