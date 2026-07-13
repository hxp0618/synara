import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ProjectId,
  type ProviderInteractionMode,
  type ProviderUserInputAnswers,
  type RuntimeMode,
} from "@synara/contracts";
import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  controlPlaneClient,
  ControlPlaneError,
  type ControlPlaneAgentSession,
  type ControlPlaneAgentTurn,
  type ControlPlaneControlCommand,
  type ControlPlaneIdempotencyOptions,
  type ControlPlaneInteractionResolution,
  type ControlPlaneOrganization,
  type ControlPlanePendingInteractionSnapshot,
  type ControlPlanePlatformProfile,
  type ControlPlaneProject,
  type ControlPlaneSessionState,
  type ControlPlaneTenantAccess,
} from "./lib/controlPlaneClient";
import {
  projectControlPlaneProjects,
  projectControlPlaneThreads,
  type ControlPlaneSessionProjection,
  type ControlPlaneStreamStatus,
} from "./lib/controlPlaneProjection";
import { ControlPlaneProjectionRuntime } from "./lib/controlPlaneProjectionRuntime";
import {
  resolveControlPlaneCapabilities,
  type ControlPlaneCapabilities,
} from "./lib/controlPlanePermissions";
import {
  cancelControlPlaneTenantSwitchQueries,
  disposeControlPlaneTenantScope,
  enqueueControlPlaneTenantSwitch,
} from "./lib/controlPlaneTenantScope";
import { randomUUID } from "./lib/utils";
import { useComposerDraftStore } from "./composerDraftStore";
import { useStore } from "./store";

export const controlPlaneQueryKeys = {
  root: ["control-plane"] as const,
  profile: ["control-plane", "platform-profile"] as const,
  session: ["control-plane", "session"] as const,
  organizations: (tenantId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations"] as const,
  projects: (tenantId: string | null, organizationId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations", organizationId, "projects"] as const,
  sessions: (tenantId: string | null, organizationId: string | null, projectIds: string) =>
    [
      "control-plane",
      "tenants",
      tenantId,
      "organizations",
      organizationId,
      "sessions",
      projectIds,
    ] as const,
  pendingInteractions: (tenantId: string | null, sessionId: string | null) =>
    ["control-plane", "tenants", tenantId, "sessions", sessionId, "pending-interactions"] as const,
};

export type ControlPlaneAvailability = "detecting" | "local" | "available" | "unavailable";
export type ControlPlaneAuthentication =
  | "unknown"
  | "unauthenticated"
  | "authenticated"
  | "error";

export type CreateControlPlaneProjectInput = {
  name: string;
  repositoryUrl?: string;
  defaultBranch?: string;
  gitCredentialId?: string;
  visibility?: ControlPlaneProject["visibility"];
  idempotencyKey?: string;
};

export type CreateControlPlaneSessionInput = {
  title: string;
  visibility?: ControlPlaneAgentSession["visibility"];
  provider: string;
  model?: string;
  providerCredentialId?: string;
  executionTargetId?: string;
  idempotencyKey?: string;
};

export type ControlPlaneContextValue = {
  availability: ControlPlaneAvailability;
  authentication: ControlPlaneAuthentication;
  isAuthoritative: boolean;
  profile: ControlPlanePlatformProfile | null;
  session: ControlPlaneSessionState | null;
  activeTenant: ControlPlaneTenantAccess | null;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  activeOrganization: ControlPlaneOrganization | null;
  projects: ReadonlyArray<ControlPlaneProject>;
  sessions: ReadonlyArray<ControlPlaneAgentSession>;
  capabilities: ControlPlaneCapabilities;
  streamStatusBySessionId: Readonly<Record<string, ControlPlaneStreamStatus>>;
  error: Error | null;
  projectionError: Error | null;
  retry: () => Promise<void>;
  devLogin: (input: { email: string; displayName: string }) => Promise<void>;
  logout: () => Promise<void>;
  setActiveTenant: (tenantId: string) => Promise<void>;
  setActiveOrganization: (organizationId: string) => void;
  createProject: (input: CreateControlPlaneProjectInput) => Promise<ControlPlaneProject>;
  createSession: (
    projectId: string,
    input: CreateControlPlaneSessionInput,
  ) => Promise<ControlPlaneAgentSession>;
  createTurn: (
    sessionId: string,
    inputText: string,
    idempotencyKey?: string,
    modes?: { runtimeMode: RuntimeMode; interactionMode: ProviderInteractionMode },
  ) => Promise<ControlPlaneAgentTurn>;
  steerActiveTurn: (
    sessionId: string,
    inputText: string,
    idempotencyKey?: string,
  ) => Promise<ControlPlaneControlCommand>;
  interruptActiveTurn: (
    sessionId: string,
    idempotencyKey?: string,
  ) => Promise<ControlPlaneControlCommand>;
  resolveApproval: (
    sessionId: string,
    executionId: string,
    requestId: string,
    decision: "accept" | "decline",
    idempotencyKey?: string,
  ) => Promise<ControlPlaneInteractionResolution>;
  resolveUserInput: (
    sessionId: string,
    executionId: string,
    requestId: string,
    answers: ProviderUserInputAnswers,
    idempotencyKey?: string,
  ) => Promise<ControlPlaneInteractionResolution>;
  watchSession: (sessionId: string) => () => void;
};

const ControlPlaneContext = createContext<ControlPlaneContextValue | null>(null);
const EMPTY_ORGANIZATIONS: ReadonlyArray<ControlPlaneOrganization> = [];
const EMPTY_PROJECTS: ReadonlyArray<ControlPlaneProject> = [];
const EMPTY_SESSIONS: ReadonlyArray<ControlPlaneAgentSession> = [];

function idempotencyOptions(
  operation: string,
  idempotencyKey?: string,
): ControlPlaneIdempotencyOptions {
  return { idempotencyKey: idempotencyKey ?? `web-${operation}-${randomUUID()}` };
}

function sameStreamStatuses(
  left: Readonly<Record<string, ControlPlaneStreamStatus>>,
  right: Readonly<Record<string, ControlPlaneStreamStatus>>,
): boolean {
  const leftKeys = Object.keys(left);
  const rightKeys = Object.keys(right);
  return (
    leftKeys.length === rightKeys.length &&
    leftKeys.every((key) => left[key] === right[key])
  );
}

function controlPlaneUnavailable(error: unknown): boolean {
  return (
    error instanceof ControlPlaneError &&
    (error.status === 502 || error.status === 503) &&
    error.code !== "control_plane_unavailable"
  );
}

export function ControlPlaneProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [organizationSelectionByTenant, setOrganizationSelectionByTenant] = useState<
    Record<string, string>
  >({});
  const [streamStatusBySessionId, setStreamStatusBySessionId] = useState<
    Readonly<Record<string, ControlPlaneStreamStatus>>
  >({});
  const [projectionError, setProjectionError] = useState<Error | null>(null);
  const resourcesRef = useRef<{
    projects: ReadonlyArray<ControlPlaneProject>;
    sessions: ReadonlyArray<ControlPlaneAgentSession>;
  }>({ projects: [], sessions: [] });
  const hadAuthoritativeProjectionRef = useRef(false);
  const authoritativeProjectionEnabledRef = useRef(false);
  const runtimeDisposeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const tenantSwitchQueueRef = useRef<Promise<void>>(Promise.resolve());
  const projectionHandlerRef = useRef<
    (projections: ReadonlyMap<string, ControlPlaneSessionProjection>) => void
  >(() => undefined);
  const [projectionRuntime] = useState(
    () =>
      new ControlPlaneProjectionRuntime({
        client: controlPlaneClient,
        onChange: (projections) => projectionHandlerRef.current(projections),
      }),
  );

  const profileQuery = useQuery({
    queryKey: controlPlaneQueryKeys.profile,
    queryFn: controlPlaneClient.getPlatformProfile,
    retry: false,
    staleTime: Infinity,
  });
  const controlPlaneConfigured = profileQuery.isSuccess;
  const sessionQuery = useQuery({
    queryKey: controlPlaneQueryKeys.session,
    queryFn: controlPlaneClient.getSession,
    enabled: controlPlaneConfigured,
    retry: false,
    staleTime: 30_000,
  });
  const session = sessionQuery.data ?? null;
  const activeTenant = useMemo(
    () =>
      session?.tenants.find((tenant) => tenant.id === session.user.activeTenantId) ?? null,
    [session?.tenants, session?.user.activeTenantId],
  );
  const activeTenantId = activeTenant?.id ?? null;
  const organizationsQuery = useQuery({
    queryKey: controlPlaneQueryKeys.organizations(activeTenantId),
    queryFn: () => controlPlaneClient.listOrganizations(activeTenantId!),
    enabled: activeTenantId !== null,
    retry: false,
  });
  const organizations = organizationsQuery.data?.items ?? EMPTY_ORGANIZATIONS;
  const selectedOrganizationId = activeTenantId
    ? organizationSelectionByTenant[activeTenantId]
    : undefined;
  const activeOrganization =
    organizations.find((organization) => organization.id === selectedOrganizationId) ??
    organizations.find((organization) => organization.kind === "root") ??
    organizations[0] ??
    null;
  const activeOrganizationId = activeOrganization?.id ?? null;
  const capabilities = useMemo(
    () => resolveControlPlaneCapabilities({ tenant: activeTenant, organization: activeOrganization }),
    [activeOrganization, activeTenant],
  );
  const projectsQuery = useQuery({
    queryKey: controlPlaneQueryKeys.projects(activeTenantId, activeOrganizationId),
    queryFn: () => controlPlaneClient.listProjects(activeTenantId!, activeOrganizationId!),
    enabled:
      activeTenantId !== null && activeOrganizationId !== null && capabilities.canReadProjects,
    retry: false,
  });
  const projects = projectsQuery.data?.items ?? EMPTY_PROJECTS;
  const projectIdsKey = projects.map((project) => project.id).sort().join(",");
  const sessionsQuery = useQuery({
    queryKey: controlPlaneQueryKeys.sessions(
      activeTenantId,
      activeOrganizationId,
      projectIdsKey,
    ),
    queryFn: async () => {
      const pages = await Promise.all(
        projects.map((project) => controlPlaneClient.listProjectSessions(project.id)),
      );
      return pages.flatMap((page) => page.items);
    },
    enabled: projectsQuery.isSuccess,
    retry: false,
  });
  const sessions = sessionsQuery.data ?? EMPTY_SESSIONS;

  let availability: ControlPlaneAvailability = "detecting";
  if (profileQuery.isSuccess) availability = "available";
  else if (
    profileQuery.error instanceof ControlPlaneError &&
    profileQuery.error.status === 503 &&
    profileQuery.error.code === "control_plane_unavailable"
  ) {
    availability = "local";
  } else if (profileQuery.isError) availability = "unavailable";
  if (controlPlaneUnavailable(sessionQuery.error)) availability = "unavailable";

  let authentication: ControlPlaneAuthentication = "unknown";
  if (sessionQuery.isSuccess) authentication = "authenticated";
  else if (
    sessionQuery.error instanceof ControlPlaneError &&
    sessionQuery.error.status === 401
  ) {
    authentication = "unauthenticated";
  } else if (sessionQuery.isError) authentication = "error";
  const isAuthoritative = availability === "available" && authentication === "authenticated";

  projectionHandlerRef.current = (projections) => {
    const statuses = Object.fromEntries(
      [...projections].map(([sessionId, projection]) => [sessionId, projection.streamStatus]),
    ) as Record<string, ControlPlaneStreamStatus>;
    setStreamStatusBySessionId((current) =>
      sameStreamStatuses(current, statuses) ? current : statuses,
    );
    if (!authoritativeProjectionEnabledRef.current) return;
    const resources = resourcesRef.current;
    const projectedProjects = projectControlPlaneProjects(
      resources.projects,
      useStore.getState().projects,
    );
    const projectedThreads = projectControlPlaneThreads(resources.sessions, projections);
    useStore.getState().syncAuthoritativeProjection(projectedProjects, projectedThreads);
  };

  useEffect(() => {
    if (!isAuthoritative || !activeTenantId || !activeOrganizationId) {
      authoritativeProjectionEnabledRef.current = false;
      resourcesRef.current = { projects: [], sessions: [] };
      projectionRuntime.setScope("", []);
      setProjectionError(null);
      if (availability === "local") {
        useStore.getState().setProjectionAuthority("local");
      }
      if (hadAuthoritativeProjectionRef.current || availability === "available") {
        useStore.getState().syncAuthoritativeProjection([], []);
      }
      hadAuthoritativeProjectionRef.current = false;
      return;
    }
    authoritativeProjectionEnabledRef.current = true;
    hadAuthoritativeProjectionRef.current = true;
    resourcesRef.current = { projects, sessions };
    projectionRuntime.setScope(`${activeTenantId}:${activeOrganizationId}`, sessions);
    setProjectionError(null);
    void projectionRuntime.catchUpAll().catch((error: unknown) => {
      setProjectionError(
        error instanceof Error ? error : new Error("Failed to restore Session Events."),
      );
    });
  }, [
    activeOrganizationId,
    activeTenantId,
    availability,
    isAuthoritative,
    projectionRuntime,
    projects,
    sessions,
  ]);

  useEffect(() => {
    if (runtimeDisposeTimerRef.current) {
      clearTimeout(runtimeDisposeTimerRef.current);
      runtimeDisposeTimerRef.current = null;
    }
    return () => {
      runtimeDisposeTimerRef.current = setTimeout(() => projectionRuntime.dispose(), 0);
    };
  }, [projectionRuntime]);

  const retry = useCallback(async () => {
    if (availability === "unavailable" || availability === "local") {
      await profileQuery.refetch();
      return;
    }
    await Promise.all([
      sessionQuery.refetch(),
      activeTenantId ? organizationsQuery.refetch() : Promise.resolve(),
      activeOrganizationId ? projectsQuery.refetch() : Promise.resolve(),
    ]);
  }, [
    activeOrganizationId,
    activeTenantId,
    availability,
    organizationsQuery,
    profileQuery,
    projectsQuery,
    sessionQuery,
  ]);

  const devLogin = useCallback(
    async (input: { email: string; displayName: string }) => {
      const nextSession = await controlPlaneClient.devLogin(input);
      queryClient.setQueryData(controlPlaneQueryKeys.session, nextSession);
    },
    [queryClient],
  );
  const logout = useCallback(async () => {
    await controlPlaneClient.logout();
    queryClient.removeQueries({ queryKey: controlPlaneQueryKeys.root });
    queryClient.setQueryData(controlPlaneQueryKeys.profile, profileQuery.data);
    await queryClient.invalidateQueries({ queryKey: controlPlaneQueryKeys.session });
  }, [profileQuery.data, queryClient]);
  const setActiveTenant = useCallback(
    (tenantId: string) => {
      return enqueueControlPlaneTenantSwitch(tenantSwitchQueueRef, async () => {
        const currentSession = queryClient.getQueryData<ControlPlaneSessionState>(
          controlPlaneQueryKeys.session,
        );
        const previousTenantId = currentSession?.user.activeTenantId ?? null;
        if (previousTenantId && previousTenantId !== tenantId) {
          await cancelControlPlaneTenantSwitchQueries(queryClient, previousTenantId);
        }
        const nextSession = await controlPlaneClient.setActiveTenant(tenantId);
        if (previousTenantId && previousTenantId !== tenantId) {
          disposeControlPlaneTenantScope({
            queryClient,
            tenantId: previousTenantId,
            currentProjectIds: useStore.getState().projects.map((project) => project.id),
            closeProjection: () => {
              authoritativeProjectionEnabledRef.current = false;
              resourcesRef.current = { projects: [], sessions: [] };
              projectionRuntime.setScope("", []);
              setStreamStatusBySessionId({});
              setProjectionError(null);
              useStore.getState().syncAuthoritativeProjection([], []);
            },
            clearProjectDrafts: (projectId) =>
              useComposerDraftStore
                .getState()
                .clearProjectDraftThreads(ProjectId.makeUnsafe(projectId)),
          });
        }
        queryClient.setQueryData(controlPlaneQueryKeys.session, nextSession);
        await queryClient.invalidateQueries({
          queryKey: controlPlaneQueryKeys.organizations(tenantId),
        });
      });
    },
    [projectionRuntime, queryClient],
  );
  const setActiveOrganization = useCallback(
    (organizationId: string) => {
      if (!activeTenantId) return;
      setOrganizationSelectionByTenant((current) => ({
        ...current,
        [activeTenantId]: organizationId,
      }));
    },
    [activeTenantId],
  );
  const createProject = useCallback(
    async (input: CreateControlPlaneProjectInput) => {
      if (!activeTenantId || !activeOrganizationId || !capabilities.canCreateProject) {
        throw new Error("The active Tenant or Organization does not allow Project creation.");
      }
      const project = await controlPlaneClient.createProject(
        activeTenantId,
        activeOrganizationId,
        {
          name: input.name,
          ...(input.repositoryUrl ? { repositoryUrl: input.repositoryUrl } : {}),
          defaultBranch: input.defaultBranch ?? "main",
          ...(input.gitCredentialId ? { gitCredentialId: input.gitCredentialId } : {}),
          visibility: input.visibility ?? "organization",
        },
        idempotencyOptions("project", input.idempotencyKey),
      );
      const nextProjects = projects.some((item) => item.id === project.id)
        ? projects.map((item) => (item.id === project.id ? project : item))
        : [...projects, project];
      resourcesRef.current = { projects: nextProjects, sessions };
      projectionHandlerRef.current(projectionRuntime.projections);
      await queryClient.invalidateQueries({
        queryKey: controlPlaneQueryKeys.projects(activeTenantId, activeOrganizationId),
      });
      return project;
    },
    [
      activeOrganizationId,
      activeTenantId,
      capabilities.canCreateProject,
      projectionRuntime,
      projects,
      queryClient,
      sessions,
    ],
  );
  const createSession = useCallback(
    async (projectId: string, input: CreateControlPlaneSessionInput) => {
      if (!capabilities.canCreateSession) {
        throw new Error("The active Tenant or Organization does not allow Session creation.");
      }
      const nextSession = await controlPlaneClient.createSession(
        projectId,
        {
          title: input.title,
          visibility: input.visibility ?? "private",
          provider: input.provider,
          ...(input.model ? { model: input.model } : {}),
          ...(input.providerCredentialId
            ? { providerCredentialId: input.providerCredentialId }
            : {}),
          ...(input.executionTargetId ? { executionTargetId: input.executionTargetId } : {}),
        },
        idempotencyOptions("session", input.idempotencyKey),
      );
      const nextSessions = sessions.some((item) => item.id === nextSession.id)
        ? sessions.map((item) => (item.id === nextSession.id ? nextSession : item))
        : [...sessions, nextSession];
      resourcesRef.current = { projects, sessions: nextSessions };
      if (activeTenantId && activeOrganizationId) {
        projectionRuntime.setScope(
          `${activeTenantId}:${activeOrganizationId}`,
          nextSessions,
        );
      }
      await sessionsQuery.refetch();
      return nextSession;
    },
    [
      activeOrganizationId,
      activeTenantId,
      capabilities.canCreateSession,
      projectionRuntime,
      projects,
      sessions,
      sessionsQuery,
    ],
  );
  const createTurn = useCallback(
    async (
      sessionId: string,
      inputText: string,
      idempotencyKey?: string,
      modes?: { runtimeMode: RuntimeMode; interactionMode: ProviderInteractionMode },
    ) => {
      if (!capabilities.canCreateTurn) {
        throw new Error("The active Tenant or Organization is read-only for new Turns.");
      }
      const turn = await controlPlaneClient.createTurn(
        sessionId,
        inputText,
        idempotencyOptions("turn", idempotencyKey),
        modes,
      );
      void projectionRuntime.catchUp(sessionId).catch(() => undefined);
      return turn;
    },
    [capabilities.canCreateTurn, projectionRuntime],
  );
  const interruptActiveTurn = useCallback(
    async (sessionId: string, idempotencyKey?: string) => {
      if (!capabilities.canInterruptExecution) {
        throw new Error("The active Tenant or Organization cannot interrupt Executions.");
      }
      const command = await controlPlaneClient.interruptActiveTurn(
        sessionId,
        idempotencyOptions("interrupt", idempotencyKey),
      );
      void projectionRuntime.catchUp(sessionId).catch(() => undefined);
      return command;
    },
    [capabilities.canInterruptExecution, projectionRuntime],
  );
  const steerActiveTurn = useCallback(
    async (sessionId: string, inputText: string, idempotencyKey?: string) => {
      if (!capabilities.canSteerExecution) {
        throw new Error("The active Tenant or Organization cannot steer Executions.");
      }
      const command = await controlPlaneClient.steerActiveTurn(
        sessionId,
        inputText,
        idempotencyOptions("steer", idempotencyKey),
      );
      void projectionRuntime.catchUp(sessionId).catch(() => undefined);
      return command;
    },
    [capabilities.canSteerExecution, projectionRuntime],
  );
  const resolveApproval = useCallback(
    async (
      sessionId: string,
      executionId: string,
      requestId: string,
      decision: "accept" | "decline",
      idempotencyKey?: string,
    ) => {
      if (!activeTenantId || !capabilities.canApproveExecution) {
        throw new Error("The active Tenant or Organization cannot resolve approvals.");
      }
      const tenantId = activeTenantId;
      try {
        const interaction = await controlPlaneClient.resolveApproval(
          executionId,
          requestId,
          decision,
          idempotencyOptions("approval", idempotencyKey),
        );
        queryClient.setQueryData<ControlPlanePendingInteractionSnapshot>(
          controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
          (current) =>
            current
              ? {
                  ...current,
                  items: current.items.filter((item) => item.id !== interaction.id),
                }
              : current,
        );
        void queryClient.invalidateQueries({
          queryKey: controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
        });
        void projectionRuntime.catchUp(sessionId).catch(() => undefined);
        return interaction;
      } catch (error) {
        void queryClient.invalidateQueries({
          queryKey: controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
        });
        throw error;
      }
    },
    [activeTenantId, capabilities.canApproveExecution, projectionRuntime, queryClient],
  );
  const resolveUserInput = useCallback(
    async (
      sessionId: string,
      executionId: string,
      requestId: string,
      answers: ProviderUserInputAnswers,
      idempotencyKey?: string,
    ) => {
      if (!activeTenantId || !capabilities.canApproveExecution) {
        throw new Error("The active Tenant or Organization cannot resolve user input.");
      }
      const tenantId = activeTenantId;
      try {
        const interaction = await controlPlaneClient.resolveUserInput(
          executionId,
          requestId,
          answers,
          idempotencyOptions("user-input", idempotencyKey),
        );
        queryClient.setQueryData<ControlPlanePendingInteractionSnapshot>(
          controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
          (current) =>
            current
              ? {
                  ...current,
                  items: current.items.filter((item) => item.id !== interaction.id),
                }
              : current,
        );
        void queryClient.invalidateQueries({
          queryKey: controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
        });
        void projectionRuntime.catchUp(sessionId).catch(() => undefined);
        return interaction;
      } catch (error) {
        void queryClient.invalidateQueries({
          queryKey: controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
        });
        throw error;
      }
    },
    [activeTenantId, capabilities.canApproveExecution, projectionRuntime, queryClient],
  );
  const watchSession = useCallback(
    (sessionId: string) => projectionRuntime.watch(sessionId),
    [projectionRuntime],
  );

  const error =
    (availability === "unavailable" ? profileQuery.error : null) ??
    (authentication === "error" ? sessionQuery.error : null) ??
    organizationsQuery.error ??
    projectsQuery.error ??
    sessionsQuery.error ??
    null;
  const value = useMemo<ControlPlaneContextValue>(
    () => ({
      availability,
      authentication,
      isAuthoritative,
      profile: profileQuery.data ?? null,
      session,
      activeTenant,
      organizations,
      activeOrganization,
      projects,
      sessions,
      capabilities,
      streamStatusBySessionId,
      error: error instanceof Error ? error : error ? new Error(String(error)) : null,
      projectionError,
      retry,
      devLogin,
      logout,
      setActiveTenant,
      setActiveOrganization,
      createProject,
      createSession,
      createTurn,
      steerActiveTurn,
      interruptActiveTurn,
      resolveApproval,
      resolveUserInput,
      watchSession,
    }),
    [
      activeOrganization,
      activeTenant,
      authentication,
      availability,
      capabilities,
      createProject,
      createSession,
      createTurn,
      steerActiveTurn,
      interruptActiveTurn,
      resolveApproval,
      resolveUserInput,
      devLogin,
      error,
      isAuthoritative,
      logout,
      organizations,
      profileQuery.data,
      projectionError,
      projects,
      retry,
      session,
      sessions,
      setActiveOrganization,
      setActiveTenant,
      streamStatusBySessionId,
      watchSession,
    ],
  );
  return createElement(ControlPlaneContext.Provider, { value }, children);
}

export function useControlPlane(): ControlPlaneContextValue {
  const context = useContext(ControlPlaneContext);
  if (!context) throw new Error("useControlPlane must be used inside ControlPlaneProvider.");
  return context;
}

export function useControlPlanePendingInteractions(
  sessionId: string | null,
  observedInteractionSequence: number | null,
) {
  const controlPlane = useControlPlane();
  const tenantId = controlPlane.activeTenant?.id ?? null;
  const lastReconciliationRef = useRef<{ sessionId: string | null; sequence: number | null }>({
    sessionId: null,
    sequence: null,
  });
  const query = useQuery({
    queryKey: controlPlaneQueryKeys.pendingInteractions(tenantId, sessionId),
    queryFn: () => controlPlaneClient.listPendingInteractions(sessionId!),
    enabled:
      controlPlane.isAuthoritative &&
      controlPlane.capabilities.canApproveExecution &&
      tenantId !== null &&
      sessionId !== null,
    retry: false,
  });
  const snapshotSequence = query.data?.snapshotSequence ?? null;
  useEffect(() => {
    if (lastReconciliationRef.current.sessionId !== sessionId) {
      lastReconciliationRef.current = { sessionId, sequence: null };
    }
    if (
      observedInteractionSequence === null ||
      (snapshotSequence !== null && observedInteractionSequence <= snapshotSequence) ||
      query.isFetching ||
      lastReconciliationRef.current.sequence === observedInteractionSequence
    ) {
      return;
    }
    lastReconciliationRef.current = { sessionId, sequence: observedInteractionSequence };
    void query.refetch();
  }, [
    observedInteractionSequence,
    query.isFetching,
    query.refetch,
    sessionId,
    snapshotSequence,
  ]);
  return query;
}
