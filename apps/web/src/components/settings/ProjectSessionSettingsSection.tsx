import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME as formGridClassName,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME as nativeSelectClassName,
  ControlPlaneFormField as FormField,
  ControlPlaneInlineError as InlineError,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import {
  controlPlaneClient,
  type ControlPlaneAgentSession,
  type ControlPlaneCredential,
  type ControlPlaneExecutionTarget,
  type ControlPlaneOrganization,
  type ControlPlaneProject,
  type ControlPlaneSessionEvent,
} from "~/lib/controlPlaneClient";
import { listUsableControlPlaneCredentials } from "~/lib/controlPlaneCredentials";
import { cn, randomUUID } from "~/lib/utils";

const projectSessionQueryKeys = {
  projects: (tenantId: string, organizationId: string | null) =>
    ["control-plane", "tenants", tenantId, "organizations", organizationId, "projects"] as const,
  sessions: (projectId: string | null) =>
    ["control-plane", "projects", projectId, "sessions"] as const,
};

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

function repositorySupportsGitCredential(repositoryUrl: string | null): boolean {
  if (!repositoryUrl) return false;
  try {
    const parsed = new URL(repositoryUrl);
    return (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === "" &&
      (parsed.port === "" || parsed.port === "443")
    );
  } catch {
    return false;
  }
}

function ProjectGitCredentialBinding(props: {
  project: ControlPlaneProject;
  credentials: ReadonlyArray<ControlPlaneCredential>;
  queryKey: ReturnType<(typeof projectSessionQueryKeys)["projects"]>;
}) {
  const queryClient = useQueryClient();
  const [credentialSelection, setCredentialSelection] = useState(
    props.project.gitCredentialId ?? "",
  );
  const updateBinding = useMutation({
    mutationFn: () =>
      controlPlaneClient.updateProject(props.project.id, {
        gitCredentialId: credentialSelection || null,
      }),
    onSuccess: (project) => {
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneProject> }>(
        props.queryKey,
        (current) => ({
          items: (current?.items ?? []).map((item) => (item.id === project.id ? project : item)),
        }),
      );
    },
  });
  const currentCredentialId = props.project.gitCredentialId;
  const repositoryConfigured = repositorySupportsGitCredential(props.project.repositoryUrl);
  const unchanged = credentialSelection === (currentCredentialId ?? "");
  const currentCredentialUnavailable =
    currentCredentialId !== null &&
    !props.credentials.some((credential) => credential.id === currentCredentialId);

  return (
    <SettingsRow
      title="Private Git access"
      description={
        repositoryConfigured
          ? "The current leased Worker resolves this Credential only for Clone and Fetch. The token is never returned to the browser."
          : "Configure an HTTPS repository before binding a private Git Credential."
      }
    >
      <form
        className={formGridClassName}
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          updateBinding.mutate();
        }}
      >
        <div className="sm:col-span-2">
          <FormField label="Git Credential">
            <select
              className={nativeSelectClassName}
              disabled={!repositoryConfigured}
              value={repositoryConfigured ? credentialSelection : ""}
              onChange={(event) => setCredentialSelection(event.target.value)}
            >
              <option value="">No Git Credential (public repository)</option>
              {currentCredentialUnavailable && currentCredentialId ? (
                <option value={currentCredentialId}>Current Git Credential is unavailable</option>
              ) : null}
              {props.credentials.map((credential) => (
                <option key={credential.id} value={credential.id}>
                  {credential.name}
                  {credential.organizationId === null ? " · tenant scope" : " · organization scope"}
                </option>
              ))}
            </select>
          </FormField>
        </div>
        <div className="sm:col-span-2">
          <Button
            disabled={!repositoryConfigured || unchanged || updateBinding.isPending}
            size="sm"
            type="submit"
          >
            {updateBinding.isPending ? "Saving Git access…" : "Save Git access"}
          </Button>
          <InlineError error={updateBinding.error} />
        </div>
      </form>
    </SettingsRow>
  );
}

export function ProjectSessionSettingsSection(props: {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  executionTargets: ReadonlyArray<ControlPlaneExecutionTarget>;
  credentials: ReadonlyArray<ControlPlaneCredential>;
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
  const [provider, setProvider] = useState("codex");
  const [providerCredentialSelection, setProviderCredentialSelection] = useState("");
  const [gitCredentialSelection, setGitCredentialSelection] = useState("");
  const [model, setModel] = useState("");
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
  });
  const compatibleGitCredentials = listUsableControlPlaneCredentials(props.credentials, {
    purpose: "git",
    organizationId: selectedOrganizationId,
  });
  const selectedProviderCredentialId = compatibleProviderCredentials.some(
    (credential) => credential.id === providerCredentialSelection,
  )
    ? providerCredentialSelection
    : null;
  const selectedGitCredentialId =
    repositorySupportsGitCredential(repositoryUrl) &&
    compatibleGitCredentials.some((credential) => credential.id === gitCredentialSelection)
      ? gitCredentialSelection
      : null;
  const watchedSession = sessions.find((session) => session.id === watchedSessionId) ?? null;
  const watchedSessionAvailable = watchedSession !== null;

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
          ...(selectedGitCredentialId ? { gitCredentialId: selectedGitCredentialId } : {}),
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
      setGitCredentialSelection("");
      setProjectSelection(project.id);
      void queryClient.invalidateQueries({
        queryKey: projectSessionQueryKeys.projects(props.tenantId, selectedOrganizationId),
      });
    },
  });
  const createSession = useMutation({
    mutationFn: () =>
      controlPlaneClient.createSession(
        selectedProjectId!,
        {
          title: sessionTitle,
          visibility: sessionVisibility,
          provider,
          ...(model ? { model } : {}),
          ...(selectedProviderCredentialId
            ? { providerCredentialId: selectedProviderCredentialId }
            : {}),
          ...(selectedExecutionTargetId ? { executionTargetId: selectedExecutionTargetId } : {}),
        },
        {
          idempotencyKey:
            sessionIdempotencyKeyRef.current ??
            (sessionIdempotencyKeyRef.current = `web-settings-session-${randomUUID()}`),
        },
      ),
    onSuccess: (session) => {
      sessionIdempotencyKeyRef.current = null;
      setSessionTitle("");
      setModel("");
      setWatchedSessionId(session.id);
      setLastLiveEvent(null);
      void queryClient.invalidateQueries({
        queryKey: projectSessionQueryKeys.sessions(selectedProjectId),
      });
    },
  });
  const createTurn = useMutation({
    mutationFn: () =>
      controlPlaneClient.createTurn(watchedSessionId!, turnInput, {
        idempotencyKey:
          turnIdempotencyKeyRef.current ??
          (turnIdempotencyKeyRef.current = `web-settings-turn-${randomUUID()}`),
      }),
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
                setGitCredentialSelection("");
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
            description={`${project.defaultBranch} · ${project.repositoryUrl ?? "No repository configured"} · ${
              project.gitCredentialId
                ? `Git Credential ${props.credentials.find((credential) => credential.id === project.gitCredentialId)?.name ?? "bound"}`
                : "No private Git Credential"
            }`}
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
                onChange={(event) => {
                  setRepositoryUrl(event.target.value);
                  if (event.target.value.trim() === "") setGitCredentialSelection("");
                }}
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
            <div className="sm:col-span-2">
              <FormField label="Git Credential">
                <select
                  className={nativeSelectClassName}
                  disabled={!repositorySupportsGitCredential(repositoryUrl)}
                  value={selectedGitCredentialId ?? ""}
                  onChange={(event) => setGitCredentialSelection(event.target.value)}
                >
                  <option value="">No Git Credential (public repository)</option>
                  {compatibleGitCredentials.map((credential) => (
                    <option key={credential.id} value={credential.id}>
                      {credential.name}
                      {credential.organizationId === null
                        ? " · tenant scope"
                        : " · organization scope"}
                    </option>
                  ))}
                </select>
              </FormField>
            </div>
            <div className="sm:col-span-2">
              <Button disabled={createProject.isPending} size="sm" type="submit">
                {createProject.isPending ? "Creating project…" : "Create project"}
              </Button>
              <InlineError error={createProject.error ?? projectsQuery.error} />
            </div>
          </form>
        </SettingsRow>
        {selectedProject ? (
          <ProjectGitCredentialBinding
            key={`${selectedProject.id}:${selectedProject.gitCredentialId ?? "none"}`}
            credentials={compatibleGitCredentials}
            project={selectedProject}
            queryKey={projectSessionQueryKeys.projects(props.tenantId, selectedOrganizationId)}
          />
        ) : null}
      </SettingsSection>

      <SettingsSection title="Agent sessions">
        {selectedProjectId ? (
          <>
            {sessions.map((session) => (
              <SettingsListRow
                key={session.id}
                title={session.title}
                description={`${session.provider}${session.model ? ` · ${session.model}` : ""}${
                  session.providerCredentialId
                    ? ` · Credential ${props.credentials.find((credential) => credential.id === session.providerCredentialId)?.name ?? session.providerCredentialId.slice(0, 8)}`
                    : " · local CLI auth"
                } · target ${session.executionTargetId.slice(0, 8)} · event sequence ${session.lastEventSequence}`}
                actions={
                  <span className="flex flex-wrap justify-end gap-1.5">
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
                    <Button disabled={createTurn.isPending} size="sm" type="submit">
                      {createTurn.isPending ? "Queueing turn…" : "Queue turn"}
                    </Button>
                    <InlineError error={createTurn.error} />
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
                  createSession.mutate();
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
                  <Input
                    required
                    value={provider}
                    onChange={(event) => {
                      setProvider(event.target.value);
                      setProviderCredentialSelection("");
                    }}
                  />
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
                <div className="sm:col-span-2">
                  <Button disabled={createSession.isPending} size="sm" type="submit">
                    {createSession.isPending ? "Creating session…" : "Create agent session"}
                  </Button>
                  <InlineError error={createSession.error ?? sessionsQuery.error} />
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
