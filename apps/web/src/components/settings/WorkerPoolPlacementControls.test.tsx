import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import {
  buildPlacementPolicyInput,
  buildWorkerPoolUpdateInput,
  buildWarmPoolInput,
  WorkerPoolPlacementControls,
  workerPoolPlacementQueryKey,
} from "./WorkerPoolPlacementControls";
import type {
  ControlPlaneExecutionPlacementState,
  ControlPlaneExecutionTarget,
} from "~/lib/controlPlaneClient";

const target: ControlPlaneExecutionTarget = {
  id: "target-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  kind: "kubernetes",
  name: "Production Kubernetes",
  status: "active",
  capabilities: {},
  createdAt: "2026-07-24T00:00:00Z",
  updatedAt: "2026-07-24T00:00:00Z",
};

const placementState: ControlPlaneExecutionPlacementState = {
  pools: [
    {
      id: "pool-default",
      tenantId: "tenant-1",
      executionTargetId: target.id,
      name: "default",
      mode: "per-execution",
      capacityClass: "standard",
      clusterId: "",
      region: "",
      namespace: "",
      desiredIdleUnits: 0,
      maxActiveUnits: 1,
      schedulingTemplate: {},
      status: "active",
      version: 1,
      createdAt: "2026-07-24T00:00:00Z",
      updatedAt: "2026-07-24T00:00:00Z",
    },
    {
      id: "pool-warm",
      tenantId: "tenant-1",
      executionTargetId: target.id,
      name: "interactive-warm",
      mode: "warm",
      capacityClass: "interactive",
      clusterId: "",
      region: "",
      namespace: "",
      desiredIdleUnits: 2,
      maxActiveUnits: 6,
      schedulingTemplate: {},
      status: "active",
      version: 3,
      createdAt: "2026-07-24T00:01:00Z",
      updatedAt: "2026-07-24T00:01:00Z",
    },
    {
      id: "pool-draining",
      tenantId: "tenant-1",
      executionTargetId: target.id,
      name: "maintenance",
      mode: "warm",
      capacityClass: "interactive",
      clusterId: "",
      region: "",
      namespace: "",
      desiredIdleUnits: 1,
      maxActiveUnits: 2,
      schedulingTemplate: {},
      status: "draining",
      version: 2,
      createdAt: "2026-07-24T00:02:00Z",
      updatedAt: "2026-07-24T00:02:00Z",
    },
  ],
  policy: {
    tenantId: "tenant-1",
    executionTargetId: target.id,
    version: 4,
    defaultPoolId: "pool-default",
    balancedPoolId: "pool-warm",
    lowLatencyPoolId: null,
    updatedBy: "user-1",
    updatedAt: "2026-07-24T00:03:00Z",
  },
};

function renderPlacementControls(state: ControlPlaneExecutionPlacementState): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(workerPoolPlacementQueryKey("tenant-1", target.id), state);
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <WorkerPoolPlacementControls canManage enabled target={target} tenantId="tenant-1" />
    </QueryClientProvider>,
  );
}

describe("WorkerPoolPlacementControls", () => {
  it("renders authoritative pool and policy state from the backend", () => {
    const markup = renderPlacementControls(placementState);

    expect(markup).toContain("Worker Pools &amp; Placement");
    expect(markup).toContain("Target-local capacity contracts");
    expect(markup).toContain("requested Session mode alone is not capacity proof");
    expect(markup).toContain("default");
    expect(markup).toContain("interactive-warm");
    expect(markup).toContain("maintenance");
    expect(markup).toContain("Policy v4");
    expect(markup).toContain("Low-latency preference");
    expect(markup).toContain("Save capacity");
    expect(markup).toContain("v3");
  });

  it("builds a warm pool create payload with explicit interactive capacity", () => {
    expect(
      buildWarmPoolInput({
        name: " interactive-warm ",
        desiredIdleUnits: "2",
        maxActiveUnits: "6",
      }),
    ).toEqual({
      name: "interactive-warm",
      mode: "warm",
      capacityClass: "interactive",
      clusterId: "",
      region: "",
      namespace: "",
      desiredIdleUnits: 2,
      maxActiveUnits: 6,
      schedulingTemplate: {},
      status: "active",
    });
  });

  it("updates only the low-latency mapping against the current backend policy version", () => {
    expect(buildPlacementPolicyInput(placementState, "lowLatencyPoolId", "pool-warm")).toEqual({
      expectedVersion: 4,
      defaultPoolId: "pool-default",
      balancedPoolId: "pool-warm",
      lowLatencyPoolId: "pool-warm",
    });
  });

  it("builds a CAS pool update without changing immutable placement identity", () => {
    expect(
      buildWorkerPoolUpdateInput(placementState.pools[1]!, {
        desiredIdleUnits: "3",
        maxActiveUnits: "8",
        status: "draining",
      }),
    ).toEqual({
      expectedVersion: 3,
      name: "interactive-warm",
      mode: "warm",
      capacityClass: "interactive",
      clusterId: "",
      region: "",
      namespace: "",
      desiredIdleUnits: 3,
      maxActiveUnits: 8,
      schedulingTemplate: {},
      status: "draining",
    });
  });
});
