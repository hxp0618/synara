import { QueryClient } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";

import {
  cancelControlPlaneTenantSwitchQueries,
  disposeControlPlaneTenantScope,
  enqueueControlPlaneTenantSwitch,
} from "./controlPlaneTenantScope";

describe("Control Plane Tenant scope cleanup", () => {
  it("cancels the shared Session query and every old-Tenant query before switching", async () => {
    const queryClient = new QueryClient();
    const cancelQueries = vi.spyOn(queryClient, "cancelQueries");

    await cancelControlPlaneTenantSwitchQueries(queryClient, "tenant-old");

    expect(cancelQueries).toHaveBeenCalledWith({ queryKey: ["control-plane", "session"] });
    expect(cancelQueries).toHaveBeenCalledWith({
      queryKey: ["control-plane", "tenants", "tenant-old"],
    });
  });

  it("prevents an old Tenant request from populating cache after cancellation", async () => {
    const queryClient = new QueryClient();
    let resolveRequest!: (value: { items: ReadonlyArray<{ id: string }> }) => void;
    const request = queryClient
      .fetchQuery({
        queryKey: ["control-plane", "tenants", "tenant-old", "organizations", "org-1", "projects"],
        queryFn: () =>
          new Promise<{ items: ReadonlyArray<{ id: string }> }>((resolve) => {
            resolveRequest = resolve;
          }),
      })
      .catch(() => undefined);
    await Promise.resolve();

    await cancelControlPlaneTenantSwitchQueries(queryClient, "tenant-old");
    resolveRequest({ items: [{ id: "stale-project" }] });
    await request;

    expect(
      queryClient.getQueryData([
        "control-plane",
        "tenants",
        "tenant-old",
        "organizations",
        "org-1",
        "projects",
      ]),
    ).toBeUndefined();
  });

  it("serializes rapid Tenant switches so the latest intent reaches the server last", async () => {
    const queue = { current: Promise.resolve() };
    const calls: string[] = [];
    let releaseFirst!: () => void;

    const first = enqueueControlPlaneTenantSwitch(queue, async () => {
      calls.push("first:start");
      await new Promise<void>((resolve) => {
        releaseFirst = resolve;
      });
      calls.push("first:end");
    });
    const second = enqueueControlPlaneTenantSwitch(queue, async () => {
      calls.push("second:start");
      calls.push("second:end");
    });
    await vi.waitFor(() => expect(calls).toEqual(["first:start"]));
    releaseFirst();
    await Promise.all([first, second]);
    expect(calls).toEqual(["first:start", "first:end", "second:start", "second:end"]);
  });

  it("closes projection, removes only the old Tenant cache, and clears all known drafts", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(
      ["control-plane", "tenants", "tenant-old", "organizations", "org-1", "projects"],
      { items: [{ id: "project-cached" }] },
    );
    queryClient.setQueryData(
      [
        "control-plane",
        "tenants",
        "tenant-old",
        "organizations",
        "org-1",
        "sessions",
        "project-cached",
      ],
      { items: [{ id: "session-must-not-be-treated-as-project" }] },
    );
    queryClient.setQueryData(
      ["control-plane", "tenants", "tenant-new", "organizations", "org-2", "projects"],
      { items: [{ id: "project-new" }] },
    );
    const calls: string[] = [];

    disposeControlPlaneTenantScope({
      queryClient,
      tenantId: "tenant-old",
      currentProjectIds: ["project-current"],
      closeProjection: () => calls.push("projection:closed"),
      clearProjectDrafts: (projectId) => calls.push(`draft:${projectId}`),
    });

    expect(calls).toEqual(["projection:closed", "draft:project-cached", "draft:project-current"]);
    expect(
      queryClient.getQueriesData({
        queryKey: ["control-plane", "tenants", "tenant-old"],
      }),
    ).toEqual([]);
    expect(
      queryClient.getQueryData([
        "control-plane",
        "tenants",
        "tenant-new",
        "organizations",
        "org-2",
        "projects",
      ]),
    ).toEqual({ items: [{ id: "project-new" }] });
  });
});
