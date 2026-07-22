import "../../index.css";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { page } from "vitest/browser";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";

import { ExecutionTargetWorkerManagement } from "./ExecutionTargetWorkerManagement";
import {
  ControlPlaneError,
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type ControlPlaneWorker,
  type ControlPlaneWorkerRevocationResult,
} from "~/lib/controlPlaneClient";

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

const activeWorker: ControlPlaneWorker = {
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
  compatibilityStatus: "compatible",
  workerReleaseStatus: "active",
  leaseSupported: true,
  fencingSupported: true,
  status: "online",
  administrativeStatus: "active",
  registeredAt: "2026-07-22T00:00:00Z",
  lastHeartbeatAt: "2026-07-22T00:01:00Z",
};

function revocationResult(): ControlPlaneWorkerRevocationResult {
  return {
    worker: {
      ...activeWorker,
      administrativeStatus: "revoked",
      revokedAt: "2026-07-22T00:02:00Z",
      revocationReason: "Confirmed Worker identity compromise",
    },
    releasedExecutionLeases: 1,
    recoveringExecutions: 1,
    outcomeUnknownExecutions: 0,
    checkpointUnconfirmedExecutions: 0,
    requeuedWorkspaceCleanups: 1,
  };
}

async function mountWorkerManagement(onWorkersChanged = vi.fn()) {
  const host = document.createElement("div");
  document.body.append(host);
  const screen = await render(
    <QueryClientProvider client={new QueryClient()}>
      <ExecutionTargetWorkerManagement
        canManage
        onWorkersChanged={onWorkersChanged}
        target={target}
        tenantId="tenant-1"
        workers={[activeWorker]}
      />
    </QueryClientProvider>,
    { container: host },
  );
  return {
    onWorkersChanged,
    async cleanup() {
      await screen.unmount();
      host.remove();
    },
  };
}

async function submitRevocation() {
  await page.getByLabelText("Open revoke controls for worker worker-1").click();
  const reason = page.getByLabelText("Revocation reason for worker worker-1");
  await expect.element(reason).toHaveAttribute("maxlength", "2000");
  await reason.fill("Confirmed Worker identity compromise");
  await page.getByLabelText("Revoke worker worker-1 at incarnation 7").click();
}

describe("ExecutionTargetWorkerManagement interactions", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  it("reuses one Idempotency-Key while automatically retrying an unknown outcome", async () => {
    const revoke = vi
      .spyOn(controlPlaneClient, "revokeWorker")
      .mockRejectedValueOnce(new TypeError("connection reset"))
      .mockResolvedValueOnce(revocationResult());
    const mounted = await mountWorkerManagement();

    try {
      await submitRevocation();
      await expect.poll(() => revoke.mock.calls.length, { timeout: 8_000 }).toBe(2);

      expect(revoke.mock.calls[0]?.[2]).toEqual({
        expectedIncarnation: 7,
        reason: "Confirmed Worker identity compromise",
      });
      expect(revoke.mock.calls[0]?.[3]?.idempotencyKey).toBe(
        revoke.mock.calls[1]?.[3]?.idempotencyKey,
      );
      await expect.element(page.getByText(/1 leases released · 1 recovering/)).toBeInTheDocument();
      expect(mounted.onWorkersChanged).toHaveBeenCalledOnce();
    } finally {
      await mounted.cleanup();
    }
  });

  it("starts a new logical operation after a definitive incarnation conflict", async () => {
    const revoke = vi
      .spyOn(controlPlaneClient, "revokeWorker")
      .mockRejectedValueOnce(
        new ControlPlaneError(
          409,
          "worker_incarnation_conflict",
          "The Worker incarnation changed before it could be revoked.",
        ),
      )
      .mockResolvedValueOnce(revocationResult());
    const mounted = await mountWorkerManagement();

    try {
      await submitRevocation();
      await expect.element(page.getByText(/incarnation changed/)).toBeInTheDocument();
      await page.getByLabelText("Revoke worker worker-1 at incarnation 7").click();
      await expect.poll(() => revoke.mock.calls.length).toBe(2);

      expect(revoke.mock.calls[0]?.[3]?.idempotencyKey).not.toBe(
        revoke.mock.calls[1]?.[3]?.idempotencyKey,
      );
      await expect.element(page.getByText(/Worker revoked/)).toBeInTheDocument();
    } finally {
      await mounted.cleanup();
    }
  });
});
