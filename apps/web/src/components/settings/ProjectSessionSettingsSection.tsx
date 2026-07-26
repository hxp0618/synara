import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_DISPLAY_NAMES,
  type ProviderKind,
} from "@synara/contracts";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME as nativeSelectClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { ProjectCredentialBindings } from "~/components/settings/ProjectCredentialBindings";
import {
  applyLifecycleOverrides,
  describeLifecycleBounds,
  formatLifecycleTimestamp,
  parseLifecycleIntegerInput,
  projectResourceLifecyclePolicyQueryKey,
  summarizeLifecycleEffective,
  summarizeLifecycleOverrides,
  TenantLifecyclePolicySettingsSection,
} from "~/components/settings/TenantLifecyclePolicySettingsSection";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { DisclosureChevron } from "~/components/ui/DisclosureChevron";
import { DisclosureRegion } from "~/components/ui/DisclosureRegion";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import {
  useControlPlaneProjectProviderCapabilities,
  useControlPlaneSessionProviderCapabilities,
} from "~/controlPlaneContext";
import {
  controlPlaneClient,
  type ControlPlaneAgentSession,
  type ControlPlaneCredential,
  type ControlPlaneExecutionTarget,
  type ControlPlaneOrganization,
  type ControlPlaneResourceLifecycleConfig,
  type ControlPlaneResourceLifecycleOverrideInput,
  type ControlPlaneResourceLifecycleWarmPoolMode,
  type ControlPlaneProject,
  type ControlPlaneSessionEvent,
} from "~/lib/controlPlaneClient";
import { listUsableControlPlaneCredentials } from "~/lib/controlPlaneCredentials";
import {
  assertControlPlaneCapabilityAllowed,
  providerCanStartSaaSSession,
  resolveControlPlaneTurnDispatchDecision,
} from "~/lib/controlPlaneProviderCapabilities";
import { cn, randomUUID } from "~/lib/utils";

const SAAS_PROVIDER_OPTIONS = PROVIDER_CAPABILITY_CATALOG.providers.map((entry) => entry.provider);

const projectSessionQueryKeys = {
  projects: (tenantId: string, organizationId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations", organizationId, "projects"] as const,
  sessions: (projectId: string | null) =>
    ["control-plane", "projects", projectId, "sessions"] as const,
};

const SESSION_QUERY_REFRESH_EVENT_TYPES = new Set<string>([
  "turn.created",
  "execution.leased",
  "execution.started",
  "execution.suspend-checkpointing",
  "execution.suspended",
  "execution.suspend-aborted",
  "execution.recovering",
  "execution.completed",
  "execution.failed",
  "execution.cancelled",
  "execution.interrupted",
  "request.opened",
  "request.resolved",
  "approval.requested",
  "approval.resolved",
  "user-input.requested",
  "user-input.resolved",
  "workspace.ready",
  "workspace.failed",
  "checkpoint.created",
  "checkpoint.ready",
  "checkpoint.failed",
  "session.suspended",
  "session.resumed",
  "session.archived",
]);

function shouldRefreshProjectSessionQuery(event: ControlPlaneSessionEvent): boolean {
  return SESSION_QUERY_REFRESH_EVENT_TYPES.has(event.eventType);
}

function ResourceStatus(props: { children: ReactNode; active?: boolean }) {
  return (
    <span
      className={cn(
        "rounded-full border px-2 py-0.5 text-[10px] font-medium",
        props.active
          ? "border-emerald-500/20 bg-emerald-500/8 text-emerald-700 dark:text-emerald-300"
          : "border-border bg-foreground/4 text-muted-foreground",
      )}
    >
      {props.children}
    </span>
  );
}

function runtimeEventLabel(eventType: string): string {
  switch (eventType) {
    case "turn.created":
      return "Turn queued";
    case "execution.leased":
      return "Worker assigned";
    case "execution.started":
      return "Running";
    case "execution.recovering":
      return "Recovering";
    case "execution.completed":
      return "Completed";
    case "execution.failed":
      return "Failed";
    case "runtime.output.delta":
      return "Output received";
    case "workspace.dirty":
      return "Workspace changed";
    case "session.created":
      return "Session created";
    case "session.archived":
      return "Session archived";
    default:
      return eventType;
  }
}

function resourceStateActive(state: ControlPlaneAgentSession["resourceState"]): boolean {
  return state === "active" || state === "restoring" || state === "checkpointing";
}

function sessionIdentityDescription(
  session: ControlPlaneAgentSession,
  credentials: ReadonlyArray<ControlPlaneCredential>,
): string {
  return `${session.provider}${session.model ? ` · ${session.model}` : ""}${
    session.providerCredentialId
      ? ` · Credential ${credentials.find((credential) => credential.id === session.providerCredentialId)?.name ?? session.providerCredentialId.slice(0, 8)}`
      : " · local CLI auth"
  } · target ${session.executionTargetId.slice(0, 8)} · event sequence ${session.lastEventSequence}`;
}

function sessionResourceDescription(session: ControlPlaneAgentSession): string {
  return [
    `State ${session.resourceState}`,
    `activity ${formatLifecycleTimestamp(session.meaningfulActivityAt)}`,
    `idle ${formatLifecycleTimestamp(session.resourceIdleSince, "not idle")}`,
    `expires ${formatLifecycleTimestamp(session.absoluteExpiresAt, "not scheduled")}`,
    `policy ${summarizeLifecycleEffective(session.resourceLifecyclePolicy)}`,
  ].join(" · ");
}

type SessionLifecycleOverrideDraft = {
  waitingKeepAliveSeconds: string;
  suspendAfterIdleSeconds: string;
  absoluteSessionLifetimeSeconds: string;
  workspaceRetentionDays: string;
  warmPoolMode: ControlPlaneResourceLifecycleWarmPoolMode | "";
};

function emptySessionLifecycleOverrideDraft(): SessionLifecycleOverrideDraft {
  return {
    waitingKeepAliveSeconds: "",
    suspendAfterIdleSeconds: "",
    absoluteSessionLifetimeSeconds: "",
    workspaceRetentionDays: "",
    warmPoolMode: "",
  };
}

export function buildSessionLifecycleOverrideInput(
  draft: SessionLifecycleOverrideDraft,
  config: ControlPlaneResourceLifecycleConfig,
): ControlPlaneResourceLifecycleOverrideInput {
  return {
    waitingKeepAliveSeconds: parseLifecycleIntegerInput(
      draft.waitingKeepAliveSeconds,
      "Waiting keep-alive",
      config.bounds.waitingKeepAliveSeconds,
    ),
    suspendAfterIdleSeconds: parseLifecycleIntegerInput(
      draft.suspendAfterIdleSeconds,
      "Suspend after idle",
      config.bounds.suspendAfterIdleSeconds,
    ),
    absoluteSessionLifetimeSeconds: parseLifecycleIntegerInput(
      draft.absoluteSessionLifetimeSeconds,
      "Absolute session lifetime",
      config.bounds.absoluteSessionLifetimeSeconds,
    ),
    workspaceRetentionDays: parseLifecycleIntegerInput(
      draft.workspaceRetentionDays,
      "Workspace retention",
      config.bounds.workspaceRetentionDays,
    ),
    warmPoolMode: draft.warmPoolMode || null,
  };
}

export function hasSessionLifecycleOverrides(
  input: ControlPlaneResourceLifecycleOverrideInput,
): boolean {
  return (
    input.waitingKeepAliveSeconds !== null ||
    input.suspendAfterIdleSeconds !== null ||
    input.absoluteSessionLifetimeSeconds !== null ||
    input.workspaceRetentionDays !== null ||
    input.warmPoolMode !== null
  );
}

export function buildCreateSessionPayload(input: {
  title: string;
  visibility: ControlPlaneAgentSession["visibility"];
  provider: ProviderKind;
  model: string;
  providerCredentialId: string | null;
  executionTargetId: string | null;
  resourceLifecyclePolicy?: ControlPlaneResourceLifecycleOverrideInput;
}): Parameters<typeof controlPlaneClient.createSession>[1] {
  return {
    title: input.title,
    visibility: input.visibility,
    provider: input.provider,
    ...(input.model ? { model: input.model } : {}),
    ...(input.providerCredentialId ? { providerCredentialId: input.providerCredentialId } : {}),
    ...(input.executionTargetId ? { executionTargetId: input.executionTargetId } : {}),
    ...(input.resourceLifecyclePolicy
      ? { resourceLifecyclePolicy: input.resourceLifecyclePolicy }
      : {}),
  };
}

export function ProjectSessionSettingsSection(props: {
  tenantId: string;
  userId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  executionTargets: ReadonlyArray<ControlPlaneExecutionTarget>;
  credentials: ReadonlyArray<ControlPlaneCredential>;
  canReadProjects: boolean;
  canManageProjectLifecycle: boolean;
  resourceLifecycleConfig: ControlPlaneResourceLifecycleConfig | null;
}) {
  const queryClient = useQueryClient();
  const [organizationSelection, setOrganizationSelection] = useState("");
  const [projectSelection, setProjectSelection] = useState("");
  const [projectName, setProjectName] = useState("");
  const [repositoryUrl, setRepositoryUrl] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [projectVisibility, setProjectVisibility] =
    useState<ControlPlaneProject["visibility"]>("organization");
  const [sessionTitle, setSessionTitle] = useState("");
  const [sessionVisibility, setSessionVisibility] =
    useState<ControlPlaneAgentSession["visibility"]>("private");
  const [executionTargetSelection, setExecutionTargetSelection] = useState("");
  const [provider, setProvider] = useState<ProviderKind>("codex");
  const [providerCredentialSelection, setProviderCredentialSelection] = useState("");
  const [model, setModel] = useState("");
  const [sessionLifecycleOverridesOpen, setSessionLifecycleOverridesOpen] = useState(false);
  const [sessionLifecycleDraft, setSessionLifecycleDraft] = useState<SessionLifecycleOverrideDraft>(
    emptySessionLifecycleOverrideDraft,
  );
  const [sessionLifecycleInputError, setSessionLifecycleInputError] = useState<unknown>(null);
  const [watchedSessionId, setWatchedSessionId] = useState<string | null>(null);
  const [turnInput, setTurnInput] = useState("");
  const [streamStatus, setStreamStatus] = useState<"idle" | "connecting" | "live" | "reconnecting">(
    "idle",
  );
  const [lastLiveEvent, setLastLiveEvent] = useState<ControlPlaneSessionEvent | null>(null);
  const projectIdempotencyKeyRef = useRef<string | null>(null);
  const sessionIdempotencyKeyRef = useRef<string | null>(null);
  const turnIdempotencyKeyRef = useRef<string | null>(null);

  const selectedOrganizationId = props.organizations.some(
    (organization) => organization.id === organizationSelection,
  )
    ? organizationSelection
    : (props.organizations[0]?.id ?? null);
  const projectsQuery = useQuery({
    queryKey: projectSessionQueryKeys.projects(props.tenantId, selectedOrganizationId),
    queryFn: () => controlPlaneClient.listProjects(props.tenantId, selectedOrganizationId!),
    enabled: selectedOrganizationId !== null,
    retry: false,
  });
  const projects = projectsQuery.data?.items ?? [];
  const selectedProjectId = projects.some((project) => project.id === projectSelection)
    ? projectSelection
    : (projects[0]?.id ?? null);
  const selectedProject = projects.find((project) => project.id === selectedProjectId) ?? null;
  const selectedProjectLifecyclePolicyQuery = useQuery({
    queryKey: projectResourceLifecyclePolicyQueryKey(selectedProjectId ?? ""),
    queryFn: () => controlPlaneClient.getProjectResourceLifecyclePolicy(selectedProjectId!),
    enabled: selectedProjectId !== null && props.canReadProjects,
    retry: false,
  });
  const sessionsQuery = useQuery({
    queryKey: projectSessionQueryKeys.sessions(selectedProjectId),
    queryFn: () => controlPlaneClient.listProjectSessions(selectedProjectId!),
    enabled: selectedProjectId !== null,
    retry: false,
  });
  const sessions = sessionsQuery.data?.items ?? [];
  const compatibleExecutionTargets = props.executionTargets.filter(
    (target) =>
      target.status === "active" &&
      (target.organizationId === null || target.organizationId === selectedOrganizationId),
  );
  const selectedExecutionTargetId = compatibleExecutionTargets.some(
    (target) => target.id === executionTargetSelection,
  )
    ? executionTargetSelection
    : (compatibleExecutionTargets[0]?.id ?? null);
  const compatibleProviderCredentials = listUsableControlPlaneCredentials(props.credentials, {
    purpose: "provider",
    provider,
    organizationId: selectedOrganizationId,
    userId: props.userId,
    model: model.trim() || null,
  });
  const selectedProviderCredentialId = compatibleProviderCredentials.some(
    (credential) => credential.id === providerCredentialSelection,
  )
    ? providerCredentialSelection
    : null;
  const watchedSession = sessions.find((session) => session.id === watchedSessionId) ?? null;
  const watchedSessionAvailable = watchedSession !== null;
  const projectProviderCapabilitiesQuery = useControlPlaneProjectProviderCapabilities(
    selectedProjectId,
    selectedExecutionTargetId,
  );
  const watchedSessionProviderCapabilitiesQuery = useControlPlaneSessionProviderCapabilities(
    watchedSession?.id ?? null,
  );
  const createSessionCapabilityDecision = providerCanStartSaaSSession({
    projection: projectProviderCapabilitiesQuery.data,
    ...(projectProviderCapabilitiesQuery.error
      ? { projectionError: projectProviderCapabilitiesQuery.error }
      : {}),
    provider,
  });
  const providerOptions = SAAS_PROVIDER_OPTIONS.map((providerOption) => ({
    provider: providerOption,
    decision: providerCanStartSaaSSession({
      projection: projectProviderCapabilitiesQuery.data,
      ...(projectProviderCapabilitiesQuery.error
        ? { projectionError: projectProviderCapabilitiesQuery.error }
        : {}),
      provider: providerOption,
    }),
  }));
  const createTurnCapabilityDecision = watchedSession
    ? resolveControlPlaneTurnDispatchDecision({
        isAuthoritative: true,
        projection: watchedSessionProviderCapabilitiesQuery.data,
        ...(watchedSessionProviderCapabilitiesQuery.error
          ? { projectionError: watchedSessionProviderCapabilitiesQuery.error }
          : {}),
        provider: watchedSession.provider,
        includeSessionStart: false,
        interactionMode: "default",
      })
    : null;

  let sessionLifecycleDraftInput: ControlPlaneResourceLifecycleOverrideInput | null = null;
  let sessionLifecycleDraftValidationError: unknown = null;
  if (props.resourceLifecycleConfig) {
    try {
      sessionLifecycleDraftInput = buildSessionLifecycleOverrideInput(
        sessionLifecycleDraft,
        props.resourceLifecycleConfig,
      );
    } catch (error) {
      sessionLifecycleDraftValidationError = error;
    }
  }
  const selectedProjectLifecycleEffective =
    selectedProjectLifecyclePolicyQuery.data?.effective ?? null;
  const sessionLifecyclePreview =
    selectedProjectLifecycleEffective && sessionLifecycleDraftInput
      ? applyLifecycleOverrides(selectedProjectLifecycleEffective, sessionLifecycleDraftInput)
      : selectedProjectLifecycleEffective;
  const sessionLifecycleSummary =
    sessionLifecycleDraftValidationError instanceof Error
      ? sessionLifecycleDraftValidationError.message
      : summarizeLifecycleOverrides(
          sessionLifecycleDraftInput ?? {
            waitingKeepAliveSeconds: null,
            suspendAfterIdleSeconds: null,
            absoluteSessionLifetimeSeconds: null,
            workspaceRetentionDays: null,
            warmPoolMode: null,
          },
          "the selected Project policy",
        );

  useEffect(() => {
    setSessionLifecycleOverridesOpen(false);
    setSessionLifecycleDraft(emptySessionLifecycleOverrideDraft());
    setSessionLifecycleInputError(null);
  }, [selectedProjectId]);

  useEffect(() => {
    if (!selectedProjectId || !watchedSessionId || !watchedSessionAvailable) {
      setStreamStatus("idle");
      return;
    }
    const queryKey = projectSessionQueryKeys.sessions(selectedProjectId);
    const cached = queryClient.getQueryData<{
      items: ReadonlyArray<ControlPlaneAgentSession>;
    }>(queryKey);
    const afterSequence =
      cached?.items.find((session) => session.id === watchedSessionId)?.lastEventSequence ?? 0;
    setStreamStatus("connecting");
    return controlPlaneClient.subscribeSessionEvents(watchedSessionId, afterSequence, {
      onOpen: () => setStreamStatus("live"),
      onError: () => setStreamStatus("reconnecting"),
      onEvent: (event) => {
        setLastLiveEvent(event);
        setStreamStatus("live");
        const previousSequence =
          queryClient
            .getQueryData<{
              items: ReadonlyArray<ControlPlaneAgentSession>;
            }>(queryKey)
            ?.items.find((session) => session.id === event.sessionId)?.lastEventSequence ?? 0;
        const advancedSequence = event.sequence > previousSequence;
        queryClient.setQueryData<{
          items: ReadonlyArray<ControlPlaneAgentSession>;
        }>(queryKey, (current) =>
          current
            ? {
                items: current.items.map((session) =>
                  session.id === event.sessionId && event.sequence > session.lastEventSequence
                    ? { ...session, lastEventSequence: event.sequence }
                    : session,
                ),
              }
            : current,
        );
        if (advancedSequence && shouldRefreshProjectSessionQuery(event)) {
          void queryClient.invalidateQueries({ queryKey });
        }
      },
    });
  }, [queryClient, selectedProjectId, watchedSessionAvailable, watchedSessionId]);

  const createProject = useMutation({
    mutationFn: () =>
      controlPlaneClient.createProject(
        props.tenantId,
        selectedOrganizationId!,
        {
          name: projectName,
          ...(repositoryUrl ? { repositoryUrl } : {}),
          defaultBranch,
          visibility: projectVisibility,
        },
        {
          idempotencyKey:
            projectIdempotencyKeyRef.current ??
            (projectIdempotencyKeyRef.current = `web-settings-project-${randomUUID()}`),
        },
      ),
    onSuccess: (project) => {
      projectIdempotencyKeyRef.current = null;
      setProjectName("");
      setRepositoryUrl("");
      setDefaultBranch("main");
      setProjectSelection(project.id);
      void queryClient.invalidateQueries({
        queryKey: projectSessionQueryKeys.projects(props.tenantId, selectedOrganizationId),
      });
    },
  });
  const createSession = useMutation({
    mutationFn: async (input: {
      resourceLifecyclePolicy?: ControlPlaneResourceLifecycleOverrideInput;
    }) => {
      const freshProjection = await controlPlaneClient.getProjectProviderCapabilities(
        selectedProjectId!,
        selectedExecutionTargetId ?? undefined,
      );
      assertControlPlaneCapabilityAllowed(
        providerCanStartSaaSSession({ projection: freshProjection, provider }),
      );
      return controlPlaneClient.createSession(
        selectedProjectId!,
        buildCreateSessionPayload({
          title: sessionTitle,
          visibility: sessionVisibility,
          provider,
          model,
          providerCredentialId: selectedProviderCredentialId,
          executionTargetId: selectedExecutionTargetId,
          ...(input.resourceLifecyclePolicy
            ? { resourceLifecyclePolicy: input.resourceLifecyclePolicy }
            : {}),
        }),
        {
          idempotencyKey:
            sessionIdempotencyKeyRef.current ??
            (sessionIdempotencyKeyRef.current = `web-settings-session-${randomUUID()}`),
        },
      );
    },
    onSuccess: (session) => {
      sessionIdempotencyKeyRef.current = null;
      setSessionTitle("");
      setModel("");
      setSessionLifecycleOverridesOpen(false);
      setSessionLifecycleDraft(emptySessionLifecycleOverrideDraft());
      setSessionLifecycleInputError(null);
      setWatchedSessionId(session.id);
      setLastLiveEvent(null);
      void queryClient.invalidateQueries({
        queryKey: projectSessionQueryKeys.sessions(selectedProjectId),
      });
    },
  });
  const createTurn = useMutation({
    mutationFn: async () => {
      if (!watchedSessionId || !watchedSession) {
        throw new Error("Select an active Session before creating a Turn.");
      }
      const freshProjection =
        await controlPlaneClient.getSessionProviderCapabilities(watchedSessionId);
      assertControlPlaneCapabilityAllowed(
        resolveControlPlaneTurnDispatchDecision({
          isAuthoritative: true,
          projection: freshProjection,
          provider: watchedSession.provider,
          includeSessionStart: false,
          interactionMode: "default",
        }),
      );
      return controlPlaneClient.createTurn(watchedSessionId!, turnInput, {
        idempotencyKey:
          turnIdempotencyKeyRef.current ??
          (turnIdempotencyKeyRef.current = `web-settings-turn-${randomUUID()}`),
      });
    },
    onSuccess: () => {
      turnIdempotencyKeyRef.current = null;
      setTurnInput("");
      void queryClient.invalidateQueries({
        queryKey: projectSessionQueryKeys.sessions(selectedProjectId),
      });
    },
  });

  if (props.organizations.length === 0) return null;

  return (
    <>
      <SettingsSection title="Projects">
        <SettingsRow
          title="Organization scope"
          description="Projects remain inside one tenant and one organization. Cross-tenant moves require an explicit migration."
          control={
            <select
              aria-label="Project organization"
              className={cn(nativeSelectClassName, "min-w-44")}
              onChange={(event) => {
                setOrganizationSelection(event.target.value);
                setProjectSelection("");
                setExecutionTargetSelection("");
                setProviderCredentialSelection("");
                setSessionLifecycleOverridesOpen(false);
                setSessionLifecycleDraft(emptySessionLifecycleOverrideDraft());
                setSessionLifecycleInputError(null);
                setWatchedSessionId(null);
                setLastLiveEvent(null);
              }}
              value={selectedOrganizationId ?? ""}
            >
              {props.organizations.map((organization) => (
                <option key={organization.id} value={organization.id}>
                  {organization.name}
                </option>
              ))}
            </select>
          }
        />
        {projects.map((project) => (
          <SettingsListRow
            key={project.id}
            title={project.name}
            description={`${project.defaultBranch} · ${project.repositoryUrl ?? "No repository configured"}`}
            actions={
              <span className="flex flex-wrap justify-end gap-1.5">
                <ResourceStatus active={project.id === selectedProjectId}>
                  {project.id === selectedProjectId ? "Selected" : project.visibility}
                </ResourceStatus>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setProjectSelection(project.id);
                    setSessionLifecycleOverridesOpen(false);
                    setSessionLifecycleDraft(emptySessionLifecycleOverrideDraft());
                    setSessionLifecycleInputError(null);
                    setWatchedSessionId(null);
                    setLastLiveEvent(null);
                  }}
                >
                  View sessions
                </Button>
              </span>
            }
          />
        ))}
        {projectsQuery.isPending ? (
          <SettingsListRow title="Loading projects…" />
        ) : projects.length === 0 ? (
          <SettingsListRow
            title="No projects in this organization"
            description="Create the first project to establish the ownership boundary for agent sessions."
          />
        ) : null}
        <SettingsRow
          title="Create project"
          description="The control plane persists ownership and repository defaults in PostgreSQL."
        >
          <form
            className={formGridClassName}
            onSubmit={(event: FormEvent) => {
              event.preventDefault();
              createProject.mutate();
            }}
          >
            <FormField label="Name">
              <Input
                required
                value={projectName}
                onChange={(event) => setProjectName(event.target.value)}
              />
            </FormField>
            <FormField label="Repository URL">
              <Input
                placeholder="https://github.com/company/project.git"
                value={repositoryUrl}
                onChange={(event) => setRepositoryUrl(event.target.value)}
              />
            </FormField>
            <FormField label="Default branch">
              <Input
                required
                value={defaultBranch}
                onChange={(event) => setDefaultBranch(event.target.value)}
              />
            </FormField>
            <FormField label="Visibility">
              <select
                className={nativeSelectClassName}
                value={projectVisibility}
                onChange={(event) =>
                  setProjectVisibility(event.target.value as ControlPlaneProject["visibility"])
                }
              >
                <option value="private">Private</option>
                <option value="organization">Organization</option>
                <option value="tenant">Tenant</option>
              </select>
            </FormField>
            <p className="text-xs text-muted-foreground sm:col-span-2">
              Create the Project first, then add immutable Git, Registry, or Package Bindings below.
            </p>
            <div className="sm:col-span-2">
              <Button disabled={createProject.isPending} size="sm" type="submit">
                {createProject.isPending ? "Creating project…" : "Create project"}
              </Button>
              <InlineError error={createProject.error ?? projectsQuery.error} />
            </div>
          </form>
        </SettingsRow>
        {selectedProject ? (
          <ProjectCredentialBindings
            key={selectedProject.id}
            credentials={props.credentials}
            project={selectedProject}
            tenantId={props.tenantId}
          />
        ) : null}
      </SettingsSection>

      {selectedProject && props.canReadProjects && props.resourceLifecycleConfig ? (
        <TenantLifecyclePolicySettingsSection
          canManage={props.canManageProjectLifecycle}
          config={props.resourceLifecycleConfig}
          projectId={selectedProject.id}
          projectName={selectedProject.name}
          scope="project"
          tenantId={props.tenantId}
        />
      ) : null}

      <SettingsSection title="Agent sessions">
        {selectedProjectId ? (
          <>
            {sessions.map((session) => (
              <SettingsListRow
                key={session.id}
                title={session.title}
                description={
                  <div className="space-y-1">
                    <div>{sessionIdentityDescription(session, props.credentials)}</div>
                    <div>{sessionResourceDescription(session)}</div>
                  </div>
                }
                actions={
                  <span className="flex flex-wrap justify-end gap-1.5">
                    <ResourceStatus active={resourceStateActive(session.resourceState)}>
                      {session.resourceState}
                    </ResourceStatus>
                    <ResourceStatus active={session.status === "active"}>
                      {session.status}
                    </ResourceStatus>
                    <ResourceStatus>{session.visibility}</ResourceStatus>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        setWatchedSessionId(session.id);
                        setLastLiveEvent(null);
                      }}
                    >
                      {watchedSessionId === session.id ? "Watching live" : "Watch live"}
                    </Button>
                  </span>
                }
              />
            ))}
            {sessionsQuery.isPending ? (
              <SettingsListRow title="Loading sessions…" />
            ) : sessions.length === 0 ? (
              <SettingsListRow
                title="No active agent sessions"
                description="Create a session to persist provider identity, visibility, turns, and ordered runtime events."
              />
            ) : null}
            {watchedSession ? (
              <SettingsRow
                title="Queue turn"
                description="The turn, queued execution, dispatch outbox, and first event commit atomically. Worker lifecycle updates then follow the durable SSE stream."
                status={
                  <span aria-live="polite" className="flex flex-wrap gap-1.5">
                    <ResourceStatus active={resourceStateActive(watchedSession.resourceState)}>
                      {watchedSession.resourceState}
                    </ResourceStatus>
                    <ResourceStatus active={streamStatus === "live"}>
                      {streamStatus === "live"
                        ? "Live"
                        : streamStatus === "reconnecting"
                          ? "Reconnecting"
                          : "Connecting"}
                    </ResourceStatus>
                    {lastLiveEvent ? (
                      <ResourceStatus>{runtimeEventLabel(lastLiveEvent.eventType)}</ResourceStatus>
                    ) : null}
                    <span className="text-[11px] text-muted-foreground">
                      activity {formatLifecycleTimestamp(watchedSession.meaningfulActivityAt)}
                    </span>
                  </span>
                }
              >
                <form
                  className="mt-3 grid gap-3"
                  onSubmit={(event: FormEvent) => {
                    event.preventDefault();
                    createTurn.mutate();
                  }}
                >
                  <FormField label="Turn input">
                    <Textarea
                      required
                      size="sm"
                      value={turnInput}
                      onChange={(event) => setTurnInput(event.target.value)}
                    />
                  </FormField>
                  <div>
                    <Button
                      disabled={
                        createTurn.isPending || createTurnCapabilityDecision?.allowed !== true
                      }
                      size="sm"
                      type="submit"
                    >
                      {createTurn.isPending ? "Queueing turn…" : "Queue turn"}
                    </Button>
                    <InlineError error={createTurn.error} />
                    {!createTurn.isPending && createTurnCapabilityDecision?.message ? (
                      <p className="mt-1 text-xs text-muted-foreground">
                        {createTurnCapabilityDecision.message}
                      </p>
                    ) : null}
                  </div>
                </form>
              </SettingsRow>
            ) : null}
            <SettingsRow
              title="Create agent session"
              description="New sessions default to private and receive their first durable runtime event in the same transaction."
            >
              <form
                className={formGridClassName}
                onSubmit={(event: FormEvent) => {
                  event.preventDefault();
                  try {
                    if (sessionLifecycleDraftValidationError) {
                      throw sessionLifecycleDraftValidationError;
                    }
                    const resourceLifecyclePolicy =
                      props.resourceLifecycleConfig && sessionLifecycleDraftInput
                        ? hasSessionLifecycleOverrides(sessionLifecycleDraftInput)
                          ? sessionLifecycleDraftInput
                          : undefined
                        : undefined;
                    setSessionLifecycleInputError(null);
                    createSession.mutate(
                      resourceLifecyclePolicy ? { resourceLifecyclePolicy } : {},
                    );
                  } catch (error) {
                    setSessionLifecycleInputError(error);
                  }
                }}
              >
                <FormField label="Title">
                  <Input
                    required
                    value={sessionTitle}
                    onChange={(event) => setSessionTitle(event.target.value)}
                  />
                </FormField>
                <FormField label="Visibility">
                  <select
                    className={nativeSelectClassName}
                    value={sessionVisibility}
                    onChange={(event) =>
                      setSessionVisibility(
                        event.target.value as ControlPlaneAgentSession["visibility"],
                      )
                    }
                  >
                    <option value="private">Private</option>
                    <option value="project">Project</option>
                    <option value="organization">Organization</option>
                  </select>
                </FormField>
                <FormField label="Provider">
                  <select
                    className={nativeSelectClassName}
                    value={provider}
                    onChange={(event) => {
                      setProvider(event.target.value as ProviderKind);
                      setProviderCredentialSelection("");
                    }}
                  >
                    {providerOptions.map((option) => (
                      <option
                        key={option.provider}
                        value={option.provider}
                        disabled={!option.decision.allowed}
                      >
                        {PROVIDER_DISPLAY_NAMES[option.provider]}
                        {option.decision.allowed
                          ? option.decision.temporary
                            ? " · waiting for Worker"
                            : ""
                          : option.decision.blockingDecision?.status === "loading"
                            ? " · checking"
                            : " · unavailable"}
                      </option>
                    ))}
                  </select>
                </FormField>
                <FormField label="Model">
                  <Input
                    placeholder="Optional"
                    value={model}
                    onChange={(event) => setModel(event.target.value)}
                  />
                </FormField>
                {compatibleExecutionTargets.length > 0 ? (
                  <FormField label="Execution target">
                    <select
                      className={nativeSelectClassName}
                      value={selectedExecutionTargetId ?? ""}
                      onChange={(event) => setExecutionTargetSelection(event.target.value)}
                    >
                      {compatibleExecutionTargets.map((target) => (
                        <option key={target.id} value={target.id}>
                          {target.name} · {target.kind}
                        </option>
                      ))}
                    </select>
                  </FormField>
                ) : null}
                <FormField label="Provider Credential">
                  <select
                    className={nativeSelectClassName}
                    value={selectedProviderCredentialId ?? ""}
                    onChange={(event) => setProviderCredentialSelection(event.target.value)}
                  >
                    <option value="">Use Worker CLI authentication</option>
                    {compatibleProviderCredentials.map((credential) => (
                      <option key={credential.id} value={credential.id}>
                        {credential.name} · {credential.credentialType.replaceAll("_", " ")}
                      </option>
                    ))}
                  </select>
                </FormField>
                {props.resourceLifecycleConfig ? (
                  <div className="sm:col-span-2">
                    <div className="rounded-lg border border-border bg-foreground/3 p-3">
                      <div className="flex flex-col gap-2.5 sm:flex-row sm:items-start sm:justify-between">
                        <div className="min-w-0 space-y-1">
                          <p className="text-xs font-medium text-foreground">
                            Session lifecycle overrides
                          </p>
                          <p className="text-xs text-muted-foreground">{sessionLifecycleSummary}</p>
                        </div>
                        <Button
                          size="sm"
                          type="button"
                          variant="outline"
                          onClick={() => setSessionLifecycleOverridesOpen((open) => !open)}
                        >
                          <DisclosureChevron
                            open={sessionLifecycleOverridesOpen}
                            className="size-3.5"
                          />
                          {sessionLifecycleOverridesOpen
                            ? "Hide overrides"
                            : "Customize for this Session"}
                        </Button>
                      </div>
                      <DisclosureRegion
                        open={sessionLifecycleOverridesOpen}
                        contentClassName="pt-3"
                      >
                        <div className={formGridClassName}>
                          <p className="text-xs text-muted-foreground sm:col-span-2">
                            Leave every field blank to inherit from the selected Project policy. The
                            control plane computes the effective Session policy at creation time.
                          </p>
                          <p className="text-xs text-muted-foreground sm:col-span-2">
                            Runtime enforces waiting keep-alive and absolute lifetime. Suspend after
                            idle is enforced for compatible Kubernetes workers with a native
                            active-turn checkpoint; workspace retention and warm pool remain Stage 4
                            preview until their provisioners ship.
                          </p>
                          <FormField label="Waiting keep-alive (seconds)">
                            <Input
                              inputMode="numeric"
                              max={props.resourceLifecycleConfig.bounds.waitingKeepAliveSeconds.max}
                              min={props.resourceLifecycleConfig.bounds.waitingKeepAliveSeconds.min}
                              placeholder="Inherit"
                              type="number"
                              value={sessionLifecycleDraft.waitingKeepAliveSeconds}
                              onChange={(event) => {
                                setSessionLifecycleDraft((draft) => ({
                                  ...draft,
                                  waitingKeepAliveSeconds: event.target.value,
                                }));
                                setSessionLifecycleInputError(null);
                              }}
                            />
                          </FormField>
                          <FormField label="Suspend after idle (seconds)">
                            <Input
                              inputMode="numeric"
                              max={props.resourceLifecycleConfig.bounds.suspendAfterIdleSeconds.max}
                              min={props.resourceLifecycleConfig.bounds.suspendAfterIdleSeconds.min}
                              placeholder="Inherit"
                              type="number"
                              value={sessionLifecycleDraft.suspendAfterIdleSeconds}
                              onChange={(event) => {
                                setSessionLifecycleDraft((draft) => ({
                                  ...draft,
                                  suspendAfterIdleSeconds: event.target.value,
                                }));
                                setSessionLifecycleInputError(null);
                              }}
                            />
                          </FormField>
                          <FormField label="Absolute session lifetime (seconds)">
                            <Input
                              inputMode="numeric"
                              max={
                                props.resourceLifecycleConfig.bounds.absoluteSessionLifetimeSeconds
                                  .max
                              }
                              min={
                                props.resourceLifecycleConfig.bounds.absoluteSessionLifetimeSeconds
                                  .min
                              }
                              placeholder="Inherit"
                              type="number"
                              value={sessionLifecycleDraft.absoluteSessionLifetimeSeconds}
                              onChange={(event) => {
                                setSessionLifecycleDraft((draft) => ({
                                  ...draft,
                                  absoluteSessionLifetimeSeconds: event.target.value,
                                }));
                                setSessionLifecycleInputError(null);
                              }}
                            />
                          </FormField>
                          <FormField label="Workspace retention (days)">
                            <Input
                              inputMode="numeric"
                              max={props.resourceLifecycleConfig.bounds.workspaceRetentionDays.max}
                              min={props.resourceLifecycleConfig.bounds.workspaceRetentionDays.min}
                              placeholder="Inherit"
                              type="number"
                              value={sessionLifecycleDraft.workspaceRetentionDays}
                              onChange={(event) => {
                                setSessionLifecycleDraft((draft) => ({
                                  ...draft,
                                  workspaceRetentionDays: event.target.value,
                                }));
                                setSessionLifecycleInputError(null);
                              }}
                            />
                          </FormField>
                          <FormField label="Warm pool mode">
                            <select
                              className={nativeSelectClassName}
                              value={sessionLifecycleDraft.warmPoolMode}
                              onChange={(event) => {
                                setSessionLifecycleDraft((draft) => ({
                                  ...draft,
                                  warmPoolMode: event.target.value as
                                    | ControlPlaneResourceLifecycleWarmPoolMode
                                    | "",
                                }));
                                setSessionLifecycleInputError(null);
                              }}
                            >
                              <option value="">Inherit</option>
                              {props.resourceLifecycleConfig.bounds.warmPoolModes.map((mode) => (
                                <option key={mode} value={mode}>
                                  {mode.replaceAll("-", " ")}
                                </option>
                              ))}
                            </select>
                          </FormField>
                          <div className="sm:col-span-2 space-y-1">
                            <p className="text-xs text-muted-foreground">
                              Bounds: {describeLifecycleBounds(props.resourceLifecycleConfig)}.
                            </p>
                            <p className="text-xs text-muted-foreground">
                              {selectedProjectLifecycleEffective
                                ? `Base Project policy: ${summarizeLifecycleEffective(selectedProjectLifecycleEffective)}.`
                                : props.canReadProjects
                                  ? selectedProjectLifecyclePolicyQuery.isPending
                                    ? "Loading the selected Project policy preview…"
                                    : selectedProjectLifecyclePolicyQuery.error
                                      ? `Project policy preview unavailable: ${selectedProjectLifecyclePolicyQuery.error instanceof Error ? selectedProjectLifecyclePolicyQuery.error.message : "The request failed."}`
                                      : "Project policy preview unavailable."
                                  : "Project policy preview requires project.read access. Blank fields still inherit server-side from the selected Project policy."}
                            </p>
                            <p className="text-xs text-muted-foreground">
                              {sessionLifecyclePreview
                                ? `Resulting Session policy: ${summarizeLifecycleEffective(sessionLifecyclePreview)}.`
                                : "Resulting Session policy is computed server-side from the selected Project policy when the Session is created."}
                            </p>
                          </div>
                        </div>
                      </DisclosureRegion>
                    </div>
                  </div>
                ) : null}
                <div className="sm:col-span-2">
                  <Button
                    disabled={createSession.isPending || !createSessionCapabilityDecision.allowed}
                    size="sm"
                    type="submit"
                  >
                    {createSession.isPending ? "Creating session…" : "Create agent session"}
                  </Button>
                  <InlineError
                    error={
                      sessionLifecycleInputError ??
                      sessionLifecycleDraftValidationError ??
                      createSession.error ??
                      sessionsQuery.error
                    }
                  />
                  {!createSession.isPending && createSessionCapabilityDecision.message ? (
                    <p className="mt-1 text-xs text-muted-foreground">
                      {createSessionCapabilityDecision.message}
                    </p>
                  ) : null}
                </div>
              </form>
            </SettingsRow>
          </>
        ) : (
          <SettingsListRow
            title="Select or create a project"
            description="Agent sessions must belong to a project before they can be created."
          />
        )}
      </SettingsSection>
    </>
  );
}
