import type { QueryClient, QueryKey } from "@tanstack/react-query";

import type { ControlPlaneProject } from "./controlPlaneClient";

const CONTROL_PLANE_SESSION_QUERY_KEY = ["control-plane", "session"] as const;

export type ControlPlaneTenantSwitchQueue = { current: Promise<void> };

export function enqueueControlPlaneTenantSwitch(
  queue: ControlPlaneTenantSwitchQueue,
  task: () => Promise<void>,
): Promise<void> {
  const operation = queue.current.catch(() => undefined).then(task);
  queue.current = operation.then(
    () => undefined,
    () => undefined,
  );
  return operation;
}

export function controlPlaneTenantQueryPrefix(tenantId: string) {
  return ["control-plane", "tenants", tenantId] as const;
}

export async function cancelControlPlaneTenantSwitchQueries(
  queryClient: QueryClient,
  tenantId: string,
): Promise<void> {
  await Promise.all([
    queryClient.cancelQueries({ queryKey: CONTROL_PLANE_SESSION_QUERY_KEY }),
    queryClient.cancelQueries({ queryKey: controlPlaneTenantQueryPrefix(tenantId) }),
  ]);
}

export function disposeControlPlaneTenantScope(input: {
  queryClient: QueryClient;
  tenantId: string;
  currentProjectIds: ReadonlyArray<string>;
  closeProjection: () => void;
  clearProjectDrafts: (projectId: string) => void;
}): void {
  const projectIds = collectTenantProjectIds(
    input.queryClient,
    input.tenantId,
    input.currentProjectIds,
  );
  input.closeProjection();
  input.queryClient.removeQueries({ queryKey: controlPlaneTenantQueryPrefix(input.tenantId) });
  for (const projectId of projectIds) input.clearProjectDrafts(projectId);
}

function collectTenantProjectIds(
  queryClient: QueryClient,
  tenantId: string,
  currentProjectIds: ReadonlyArray<string>,
): ReadonlyArray<string> {
  const projectIds = new Set(currentProjectIds);
  for (const [queryKey, data] of queryClient.getQueriesData<{
    items?: ReadonlyArray<ControlPlaneProject>;
  }>({ queryKey: controlPlaneTenantQueryPrefix(tenantId) })) {
    if (!isTenantProjectsQuery(queryKey, tenantId) || !Array.isArray(data?.items)) continue;
    for (const project of data.items) {
      if (typeof project?.id === "string" && project.id.length > 0) projectIds.add(project.id);
    }
  }
  return [...projectIds].sort();
}

function isTenantProjectsQuery(queryKey: QueryKey, tenantId: string): boolean {
  return (
    queryKey.length >= 6 &&
    queryKey[0] === "control-plane" &&
    queryKey[1] === "tenants" &&
    queryKey[2] === tenantId &&
    queryKey.at(-1) === "projects"
  );
}
