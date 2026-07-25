import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("~/controlPlaneContext", () => ({
  useControlPlaneProjectProviderCapabilities: () => ({
    data: {
      basis: "target",
      executionTargetId: "target-1",
      targetKind: "docker",
      items: [],
    },
    error: null,
  }),
  useControlPlaneSessionProviderCapabilities: () => ({
    data: null,
    error: null,
  }),
}));

import {
  buildCreateSessionPayload,
  buildSessionLifecycleOverrideInput,
  hasSessionLifecycleOverrides,
  ProjectSessionSettingsSection,
} from "./ProjectSessionSettingsSection";
import { projectResourceLifecyclePolicyQueryKey } from "./TenantLifecyclePolicySettingsSection";
import type {
  ControlPlaneAgentSession,
  ControlPlaneExecutionTarget,
  ControlPlaneOrganization,
  ControlPlaneProject,
  ControlPlaneResourceLifecycleConfig,
  ControlPlaneResourceLifecyclePolicy,
  ControlPlaneSessionEvent,
} from "~/lib/controlPlaneClient";

const organization: ControlPlaneOrganization = {
  id: "organization-1",
  tenantId: "tenant-1",
  parentOrganizationId: null,
  slug: "platform",
  name: "Platform",
  kind: "root",
  status: "active",
  currentUserRole: "owner",
  settings: {},
  createdAt: "2026-07-24T00:00:00Z",
  updatedAt: "2026-07-24T00:00:00Z",
  archivedAt: null,
};

const executionTarget: ControlPlaneExecutionTarget = {
  id: "target-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  kind: "docker",
  name: "Docker workers",
  status: "active",
  capabilities: {},
  createdAt: "2026-07-24T00:00:00Z",
  updatedAt: "2026-07-24T00:00:00Z",
};

const project: ControlPlaneProject = {
  id: "project-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  name: "Lifecycle demo",
  repositoryUrl: "https://github.com/synara/lifecycle-demo.git",
  defaultBranch: "main",
  gitCredentialId: null,
  visibility: "organization",
  createdBy: "user-1",
  createdAt: "2026-07-24T00:00:00Z",
  updatedAt: "2026-07-24T00:00:00Z",
  archivedAt: null,
};

const session: ControlPlaneAgentSession = {
  id: "session-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  projectId: "project-1",
  createdBy: "user-1",
  title: "Persistent lifecycle session",
  status: "active",
  visibility: "private",
  provider: "codex",
  model: "gpt-5.6-sol",
  providerCredentialId: null,
  executionTargetId: "target-1",
  lastEventSequence: 9,
  resourceState: "active",
  meaningfulActivityAt: "2026-07-24T08:30:00Z",
  resourceIdleSince: "2026-07-24T08:45:00Z",
  absoluteExpiresAt: "2026-07-24T12:00:00Z",
  resourceLifecyclePolicy: {
    waitingKeepAliveSeconds: 900,
    suspendAfterIdleSeconds: 1800,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 30,
    warmPoolMode: "balanced",
  },
  createdAt: "2026-07-24T08:00:00Z",
  updatedAt: "2026-07-24T08:45:00Z",
  archivedAt: null,
};

const resourceLifecycleConfig: ControlPlaneResourceLifecycleConfig = {
  defaults: {
    waitingKeepAliveSeconds: 900,
    suspendAfterIdleSeconds: 1800,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 30,
    warmPoolMode: "balanced",
  },
  bounds: {
    waitingKeepAliveSeconds: { min: 60, max: 86_400 },
    suspendAfterIdleSeconds: { min: 60, max: 604_800 },
    absoluteSessionLifetimeSeconds: { min: 3_600, max: 31_536_000 },
    workspaceRetentionDays: { min: 1, max: 3_650 },
    warmPoolModes: ["disabled", "balanced", "low-latency"],
  },
};

const projectPolicy: ControlPlaneResourceLifecyclePolicy = {
  scope: "project",
  tenantId: "tenant-1",
  projectId: "project-1",
  overrides: {
    waitingKeepAliveSeconds: 600,
    suspendAfterIdleSeconds: null,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: null,
    warmPoolMode: null,
  },
  effective: {
    waitingKeepAliveSeconds: 600,
    suspendAfterIdleSeconds: 1800,
    absoluteSessionLifetimeSeconds: null,
    workspaceRetentionDays: 30,
    warmPoolMode: "balanced",
  },
  version: 2,
  updatedBy: "user-1",
  createdAt: "2026-07-24T01:00:00Z",
  updatedAt: "2026-07-24T02:00:00Z",
};

type SessionStreamHandlers = {
  onEvent: (event: ControlPlaneSessionEvent) => void;
  onOpen?: () => void;
  onError?: () => void;
};

type ElementLike = {
  type: unknown;
  props: Record<string, unknown> & { children?: ReactNode };
};

function isElementLike(node: ReactNode): node is ElementLike {
  return typeof node === "object" && node !== null && "props" in node;
}

function nodeChildren(node: ReactNode): ReactNode[] {
  if (Array.isArray(node)) return node;
  return node === undefined || node === null ? [] : [node];
}

function elementText(node: ReactNode): string {
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map((child) => elementText(child)).join("");
  if (!isElementLike(node)) return "";
  return elementText(node.props.children);
}

function visitElements(node: ReactNode, visitor: (element: ElementLike) => void) {
  if (Array.isArray(node)) {
    node.forEach((child) => visitElements(child, visitor));
    return;
  }
  if (!isElementLike(node)) return;
  visitor(node);
  visitElements(node.props.children, visitor);
}

function findElement(
  node: ReactNode,
  predicate: (element: ElementLike) => boolean,
): ElementLike | null {
  let match: ElementLike | null = null;
  visitElements(node, (element) => {
    if (!match && predicate(element)) {
      match = element;
    }
  });
  return match;
}

function getElement(node: ReactNode, predicate: (element: ElementLike) => boolean): ElementLike {
  const match = findElement(node, predicate);
  expect(match).not.toBeNull();
  return match!;
}

function getFirstElementChild(node: ReactNode): ElementLike {
  const child = nodeChildren(node).find((value) => isElementLike(value));
  expect(child).toBeDefined();
  return child as ElementLike;
}

function getFormFieldControl(tree: ReactNode, label: string): ElementLike {
  return getFirstElementChild(getElement(tree, (element) => element.props.label === label).props.children);
}

function getClickableElement(tree: ReactNode, text: string): ElementLike {
  return getElement(
    tree,
    (element) =>
      typeof element.props.onClick === "function" && elementText(element.props.children).includes(text),
  );
}

function getSubmitForm(tree: ReactNode, buttonText: string): ElementLike {
  return getElement(
    tree,
    (element) => element.type === "form" && elementText(element.props.children).includes(buttonText),
  );
}

function changeControlValue(control: ElementLike, value: string) {
  const onChange = control.props.onChange;
  expect(typeof onChange).toBe("function");
  (onChange as (event: { target: { value: string } }) => void)({ target: { value } });
}

function clickControl(control: ElementLike) {
  const onClick = control.props.onClick;
  expect(typeof onClick).toBe("function");
  (onClick as () => void)();
}

function submitForm(form: ElementLike) {
  const onSubmit = form.props.onSubmit;
  expect(typeof onSubmit).toBe("function");
  (onSubmit as (event: { preventDefault: () => void }) => void)({
    preventDefault: () => undefined,
  });
}

function renderProjectSessions(input?: { canReadProjects?: boolean }): string {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(
    ["control-plane", "tenants", "tenant-1", "organizations", "organization-1", "projects"],
    { items: [project] },
  );
  queryClient.setQueryData(["control-plane", "projects", "project-1", "sessions"], {
    items: [session],
  });
  if (input?.canReadProjects !== false) {
    queryClient.setQueryData(projectResourceLifecyclePolicyQueryKey("project-1"), projectPolicy);
  }

  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <ProjectSessionSettingsSection
        canManageProjectLifecycle={false}
        canReadProjects={input?.canReadProjects ?? true}
        credentials={[]}
        executionTargets={[executionTarget]}
        organizations={[organization]}
        resourceLifecycleConfig={resourceLifecycleConfig}
        tenantId="tenant-1"
        userId="user-1"
      />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.resetModules();
});

async function createSubmitPathHarness() {
  vi.resetModules();

  const stateSlots: unknown[] = [];
  const refSlots: Array<{ current: unknown }> = [];
  const pendingMutations: Array<Promise<unknown>> = [];
  let hookCursor = 0;

  const queryClient = {
    invalidateQueries: vi.fn(() => Promise.resolve()),
    getQueryData: vi.fn(() => undefined),
    setQueryData: vi.fn(),
  };
  const getProjectProviderCapabilities = vi.fn(async () => ({
    basis: "target" as const,
    executionTargetId: "target-1",
    targetKind: "docker" as const,
    items: [],
  }));
  const createSession = vi.fn(
    async (
      _projectId: string,
      input: Record<string, unknown>,
      _options?: { idempotencyKey?: string },
    ) =>
      ({
        ...session,
        id: "session-new",
        title: String(input.title),
      }) satisfies ControlPlaneAgentSession,
  );

  vi.doMock("react", async (importOriginal) => {
    const actual = await importOriginal<typeof import("react")>();
    return {
      ...actual,
      useState<T>(initial: T | (() => T)) {
        const slot = hookCursor++;
        if (!(slot in stateSlots)) {
          stateSlots[slot] = typeof initial === "function" ? (initial as () => T)() : initial;
        }
        const setState = (value: T | ((previous: T) => T)) => {
          const previous = stateSlots[slot] as T;
          stateSlots[slot] =
            typeof value === "function" ? (value as (previous: T) => T)(previous) : value;
        };
        return [stateSlots[slot] as T, setState] as const;
      },
      useRef<T>(initial: T) {
        const slot = hookCursor++;
        if (!(slot in refSlots)) {
          refSlots[slot] = { current: initial };
        }
        return refSlots[slot] as { current: T };
      },
      useEffect: vi.fn(),
    };
  });

  vi.doMock("@tanstack/react-query", async (importOriginal) => {
    const actual = await importOriginal<typeof import("@tanstack/react-query")>();
    return {
      ...actual,
      useQueryClient: () => queryClient,
      useQuery: ({ queryKey }: { queryKey: ReadonlyArray<unknown> }) => {
        const key = JSON.stringify(queryKey);
        if (
          key ===
          JSON.stringify([
            "control-plane",
            "tenants",
            "tenant-1",
            "organizations",
            "organization-1",
            "projects",
          ])
        ) {
          return { data: { items: [project] }, isPending: false, error: null, refetch: vi.fn() };
        }
        if (key === JSON.stringify(["control-plane", "projects", "project-1", "sessions"])) {
          return { data: { items: [session] }, isPending: false, error: null, refetch: vi.fn() };
        }
        if (key === JSON.stringify(projectResourceLifecyclePolicyQueryKey("project-1"))) {
          return { data: projectPolicy, isPending: false, error: null, refetch: vi.fn() };
        }
        throw new Error(`Unexpected query key ${key}`);
      },
      useMutation: (options: {
        mutationFn: (input: unknown) => Promise<unknown> | unknown;
        onSuccess?: (result: unknown) => void;
      }) => ({
        isPending: false,
        error: null,
        mutate: (input: unknown) => {
          const mutation = Promise.resolve(options.mutationFn(input)).then((result) => {
            options.onSuccess?.(result);
            return result;
          });
          pendingMutations.push(mutation);
          return mutation;
        },
      }),
    };
  });

  vi.doMock("~/controlPlaneContext", () => ({
    useControlPlaneProjectProviderCapabilities: () => ({
      data: {
        basis: "target",
        executionTargetId: "target-1",
        targetKind: "docker",
        items: [],
      },
      error: null,
    }),
    useControlPlaneSessionProviderCapabilities: () => ({
      data: null,
      error: null,
    }),
  }));

  vi.doMock("~/lib/controlPlaneClient", async (importOriginal) => {
    const actual = await importOriginal<typeof import("~/lib/controlPlaneClient")>();
    return {
      ...actual,
      controlPlaneClient: {
        ...actual.controlPlaneClient,
        createSession,
        getProjectProviderCapabilities,
        subscribeSessionEvents: vi.fn(),
      },
    };
  });

  vi.doMock("~/lib/controlPlaneProviderCapabilities", async (importOriginal) => {
    const actual = await importOriginal<typeof import("~/lib/controlPlaneProviderCapabilities")>();
    return {
      ...actual,
      assertControlPlaneCapabilityAllowed: (decision: { allowed: boolean; message?: string }) => {
        if (!decision.allowed) {
          throw new Error(decision.message ?? "blocked");
        }
      },
      providerCanStartSaaSSession: () => ({ allowed: true, temporary: false }),
      resolveControlPlaneTurnDispatchDecision: () => ({ allowed: true }),
    };
  });

  const { ProjectSessionSettingsSection: SubmitHarnessComponent } = await import(
    "./ProjectSessionSettingsSection"
  );

  return {
    createSession,
    getProjectProviderCapabilities,
    queryClient,
    render() {
      hookCursor = 0;
      return SubmitHarnessComponent({
        canManageProjectLifecycle: false,
        canReadProjects: true,
        credentials: [],
        executionTargets: [executionTarget],
        organizations: [organization],
        resourceLifecycleConfig,
        tenantId: "tenant-1",
        userId: "user-1",
      });
    },
    async flushMutations() {
      await Promise.all(pendingMutations.splice(0));
    },
  };
}

function sessionStreamEvent(eventType: string, sequence = 10): ControlPlaneSessionEvent {
  return {
    eventId: `event-${sequence}-${eventType}`,
    eventVersion: 1,
    tenantId: "tenant-1",
    organizationId: "organization-1",
    projectId: "project-1",
    sessionId: "session-1",
    executionId: "execution-1",
    workerId: "worker-1",
    generation: 1,
    sequence,
    eventType,
    actorType: "system",
    actorId: null,
    payload: {},
    occurredAt: "2026-07-24T09:00:00Z",
  };
}

async function createLiveSessionStreamHarness() {
  vi.resetModules();

  const stateSlots: unknown[] = [];
  const refSlots: Array<{ current: unknown }> = [];
  const watchedSessionStateSlot = 15;
  let hookCursor = 0;
  let pendingEffects: Array<() => void | (() => void)> = [];
  let sessionItems: ControlPlaneAgentSession[] = [session];
  let handlers: SessionStreamHandlers | null = null;

  const sessionsQueryKey = ["control-plane", "projects", "project-1", "sessions"];
  const sessionsQueryKeyJson = JSON.stringify(sessionsQueryKey);
  const projectsQueryKeyJson = JSON.stringify([
    "control-plane",
    "tenants",
    "tenant-1",
    "organizations",
    "organization-1",
    "projects",
  ]);
  const projectPolicyQueryKeyJson = JSON.stringify(projectResourceLifecyclePolicyQueryKey("project-1"));

  const queryClient = {
    invalidateQueries: vi.fn(() => Promise.resolve()),
    getQueryData: vi.fn((queryKey: ReadonlyArray<unknown>) => {
      if (JSON.stringify(queryKey) === sessionsQueryKeyJson) {
        return { items: sessionItems };
      }
      return undefined;
    }),
    setQueryData: vi.fn(
      (
        queryKey: ReadonlyArray<unknown>,
        updater:
          | { items: ReadonlyArray<ControlPlaneAgentSession> }
          | ((
              current: { items: ReadonlyArray<ControlPlaneAgentSession> } | undefined,
            ) => { items: ReadonlyArray<ControlPlaneAgentSession> } | undefined),
      ) => {
        if (JSON.stringify(queryKey) !== sessionsQueryKeyJson) {
          return undefined;
        }
        const next =
          typeof updater === "function" ? updater({ items: sessionItems }) : updater;
        if (next?.items) {
          sessionItems = [...next.items];
        }
        return next;
      },
    ),
  };

  const subscribeSessionEvents = vi.fn(
    (_sessionId: string, _afterSequence: number, nextHandlers: SessionStreamHandlers) => {
      handlers = nextHandlers;
      return () => undefined;
    },
  );

  vi.doMock("react", async (importOriginal) => {
    const actual = await importOriginal<typeof import("react")>();
    return {
      ...actual,
      useState<T>(initial: T | (() => T)) {
        const slot = hookCursor++;
        if (!(slot in stateSlots)) {
          stateSlots[slot] = typeof initial === "function" ? (initial as () => T)() : initial;
        }
        const setState = (value: T | ((previous: T) => T)) => {
          const previous = stateSlots[slot] as T;
          stateSlots[slot] =
            typeof value === "function" ? (value as (previous: T) => T)(previous) : value;
        };
        return [stateSlots[slot] as T, setState] as const;
      },
      useRef<T>(initial: T) {
        const slot = hookCursor++;
        if (!(slot in refSlots)) {
          refSlots[slot] = { current: initial };
        }
        return refSlots[slot] as { current: T };
      },
      useEffect(effect: () => void | (() => void)) {
        pendingEffects.push(effect);
      },
    };
  });

  vi.doMock("@tanstack/react-query", async (importOriginal) => {
    const actual = await importOriginal<typeof import("@tanstack/react-query")>();
    return {
      ...actual,
      useQueryClient: () => queryClient,
      useQuery: ({ queryKey }: { queryKey: ReadonlyArray<unknown> }) => {
        const key = JSON.stringify(queryKey);
        if (key === projectsQueryKeyJson) {
          return { data: { items: [project] }, isPending: false, error: null, refetch: vi.fn() };
        }
        if (key === sessionsQueryKeyJson) {
          return {
            data: { items: sessionItems },
            isPending: false,
            error: null,
            refetch: vi.fn(),
          };
        }
        if (key === projectPolicyQueryKeyJson) {
          return { data: projectPolicy, isPending: false, error: null, refetch: vi.fn() };
        }
        throw new Error(`Unexpected query key ${key}`);
      },
      useMutation: () => ({
        isPending: false,
        error: null,
        mutate: vi.fn(),
      }),
    };
  });

  vi.doMock("~/controlPlaneContext", () => ({
    useControlPlaneProjectProviderCapabilities: () => ({
      data: {
        basis: "target",
        executionTargetId: "target-1",
        targetKind: "docker",
        items: [],
      },
      error: null,
    }),
    useControlPlaneSessionProviderCapabilities: () => ({
      data: null,
      error: null,
    }),
  }));

  vi.doMock("~/lib/controlPlaneClient", async (importOriginal) => {
    const actual = await importOriginal<typeof import("~/lib/controlPlaneClient")>();
    return {
      ...actual,
      controlPlaneClient: {
        ...actual.controlPlaneClient,
        subscribeSessionEvents,
      },
    };
  });

  vi.doMock("~/lib/controlPlaneProviderCapabilities", async (importOriginal) => {
    const actual = await importOriginal<typeof import("~/lib/controlPlaneProviderCapabilities")>();
    return {
      ...actual,
      assertControlPlaneCapabilityAllowed: (decision: { allowed: boolean; message?: string }) => {
        if (!decision.allowed) {
          throw new Error(decision.message ?? "blocked");
        }
      },
      providerCanStartSaaSSession: () => ({ allowed: true, temporary: false }),
      resolveControlPlaneTurnDispatchDecision: () => ({ allowed: true }),
    };
  });

  const { ProjectSessionSettingsSection: StreamHarnessComponent } = await import(
    "./ProjectSessionSettingsSection"
  );

  return {
    queryClient,
    subscribeSessionEvents,
    getSessionItems() {
      return sessionItems;
    },
    watchSession(sessionId = "session-1") {
      stateSlots[watchedSessionStateSlot] = sessionId;
    },
    render() {
      hookCursor = 0;
      pendingEffects = [];
      return StreamHarnessComponent({
        canManageProjectLifecycle: false,
        canReadProjects: true,
        credentials: [],
        executionTargets: [executionTarget],
        organizations: [organization],
        resourceLifecycleConfig,
        tenantId: "tenant-1",
        userId: "user-1",
      });
    },
    flushEffects() {
      const effects = [...pendingEffects];
      pendingEffects = [];
      effects.forEach((effect) => effect());
    },
    emitEvent(eventType: string, sequence = 10) {
      expect(handlers).not.toBeNull();
      handlers!.onEvent(sessionStreamEvent(eventType, sequence));
    },
  };
}

describe("ProjectSessionSettingsSection", () => {
  it("builds inherit-via-null session override input for create-session requests", () => {
    const inherited = buildSessionLifecycleOverrideInput(
      {
        waitingKeepAliveSeconds: "",
        suspendAfterIdleSeconds: "",
        absoluteSessionLifetimeSeconds: "",
        workspaceRetentionDays: "",
        warmPoolMode: "",
      },
      resourceLifecycleConfig,
    );
    const customized = buildSessionLifecycleOverrideInput(
      {
        waitingKeepAliveSeconds: "600",
        suspendAfterIdleSeconds: "",
        absoluteSessionLifetimeSeconds: "86400",
        workspaceRetentionDays: "14",
        warmPoolMode: "low-latency",
      },
      resourceLifecycleConfig,
    );

    expect(inherited).toEqual({
      waitingKeepAliveSeconds: null,
      suspendAfterIdleSeconds: null,
      absoluteSessionLifetimeSeconds: null,
      workspaceRetentionDays: null,
      warmPoolMode: null,
    });
    expect(hasSessionLifecycleOverrides(inherited)).toBe(false);
    expect(customized).toEqual({
      waitingKeepAliveSeconds: 600,
      suspendAfterIdleSeconds: null,
      absoluteSessionLifetimeSeconds: 86_400,
      workspaceRetentionDays: 14,
      warmPoolMode: "low-latency",
    });
    expect(hasSessionLifecycleOverrides(customized)).toBe(true);

    const baseCreateInput = {
      title: "Lifecycle session",
      visibility: "private" as const,
      provider: "codex" as const,
      model: "",
      providerCredentialId: null,
      executionTargetId: "target-1",
    };
    expect(buildCreateSessionPayload(baseCreateInput)).not.toHaveProperty(
      "resourceLifecyclePolicy",
    );
    expect(
      buildCreateSessionPayload({
        ...baseCreateInput,
        resourceLifecyclePolicy: customized,
      }),
    ).toMatchObject({ resourceLifecyclePolicy: customized });

    expect(() =>
      buildSessionLifecycleOverrideInput(
        {
          waitingKeepAliveSeconds: "59",
          suspendAfterIdleSeconds: "",
          absoluteSessionLifetimeSeconds: "",
          workspaceRetentionDays: "",
          warmPoolMode: "",
        },
        resourceLifecycleConfig,
      ),
    ).toThrow("Waiting keep-alive must be between");
  });

  it("renders session lifecycle timestamps, effective policy, and project lifecycle policy", () => {
    const markup = renderProjectSessions();

    expect(markup).toContain("Project resource lifecycle");
    expect(markup).toContain("Persistent lifecycle session");
    expect(markup).toContain("State active");
    expect(markup).toContain("activity 2026-07-24 08:30 UTC");
    expect(markup).toContain("idle 2026-07-24 08:45 UTC");
    expect(markup).toContain("expires 2026-07-24 12:00 UTC");
    expect(markup).toContain(
      "policy wait 15m · suspend 30m · absolute No absolute expiry · retain 30d · warm balanced",
    );
    expect(markup).toContain("Current overrides");
    expect(markup).toContain("wait 10m");
    expect(markup).toContain("Session lifecycle overrides");
    expect(markup).toContain("Customize for this Session");
    expect(markup).toContain(
      "Leave every field blank to inherit from the selected Project policy. The control plane computes the effective Session policy at creation time.",
    );
    expect(markup).toContain(
      "Runtime enforces waiting keep-alive and absolute lifetime. Suspend after idle is enforced for compatible Kubernetes workers with a native active-turn checkpoint; workspace retention and warm pool remain Stage 4 preview until their provisioners ship.",
    );
    expect(markup).toContain(
      "Base Project policy: wait 10m · suspend 30m · absolute No absolute expiry · retain 30d · warm balanced.",
    );
    expect(markup).toContain(
      "Resulting Session policy: wait 10m · suspend 30m · absolute No absolute expiry · retain 30d · warm balanced.",
    );
  });

  it("keeps the session override entry visible when project policy preview is unavailable", () => {
    const markup = renderProjectSessions({ canReadProjects: false });

    expect(markup).toContain("Session lifecycle overrides");
    expect(markup).toContain(
      "Project policy preview requires project.read access. Blank fields still inherit server-side from the selected Project policy.",
    );
    expect(markup).toContain(
      "Runtime enforces waiting keep-alive and absolute lifetime. Suspend after idle is enforced for compatible Kubernetes workers with a native active-turn checkpoint; workspace retention and warm pool remain Stage 4 preview until their provisioners ship.",
    );
    expect(markup).toContain(
      "Resulting Session policy is computed server-side from the selected Project policy when the Session is created.",
    );
  });

  it.each([
    "session.suspended",
    "checkpoint.ready",
    "execution.suspend-checkpointing",
    "execution.suspended",
    "execution.suspend-aborted",
    "execution.recovering",
    "execution.completed",
  ])("refreshes the watched session query when %s arrives on the live stream", async (eventType) => {
    const harness = await createLiveSessionStreamHarness();

    harness.render();
    harness.watchSession();
    harness.render();
    harness.flushEffects();

    expect(harness.subscribeSessionEvents).toHaveBeenCalledWith(
      "session-1",
      9,
      expect.any(Object),
    );

    harness.emitEvent(eventType, 10);

    expect(harness.queryClient.invalidateQueries).toHaveBeenCalledWith({
      queryKey: ["control-plane", "projects", "project-1", "sessions"],
    });
    expect(harness.getSessionItems()[0]?.lastEventSequence).toBe(10);
  });

  it.each(["content.delta", "runtime.output.delta"])(
    "does not refresh the watched session query for %s",
    async (eventType) => {
      const harness = await createLiveSessionStreamHarness();

      harness.render();
      harness.watchSession();
      harness.render();
      harness.flushEffects();

      harness.emitEvent(eventType, 10);

      expect(harness.queryClient.invalidateQueries).not.toHaveBeenCalled();
      expect(harness.getSessionItems()[0]?.lastEventSequence).toBe(10);
    },
  );

  it("omits resourceLifecyclePolicy when every session override field is blank", async () => {
    const harness = await createSubmitPathHarness();

    let tree = harness.render();
    changeControlValue(getFormFieldControl(tree, "Title"), "Lifecycle session");
    clickControl(getClickableElement(tree, "Customize for this Session"));

    tree = harness.render();
    submitForm(getSubmitForm(tree, "Create agent session"));
    await harness.flushMutations();

    expect(harness.getProjectProviderCapabilities).toHaveBeenCalledWith("project-1", "target-1");
    expect(harness.createSession).toHaveBeenCalledTimes(1);
    expect(harness.queryClient.invalidateQueries).toHaveBeenCalledWith({
      queryKey: ["control-plane", "projects", "project-1", "sessions"],
    });
    expect(harness.createSession.mock.calls[0]?.[1]).toMatchObject({
      title: "Lifecycle session",
      visibility: "private",
      provider: "codex",
      executionTargetId: "target-1",
    });
    expect(harness.createSession.mock.calls[0]?.[1]).not.toHaveProperty("resourceLifecyclePolicy");
  });

  it("forwards session lifecycle overrides from the submitted form payload", async () => {
    const harness = await createSubmitPathHarness();

    let tree = harness.render();
    changeControlValue(getFormFieldControl(tree, "Title"), "Pinned lifecycle session");
    clickControl(getClickableElement(tree, "Customize for this Session"));

    tree = harness.render();
    changeControlValue(getFormFieldControl(tree, "Waiting keep-alive (seconds)"), "600");
    changeControlValue(getFormFieldControl(tree, "Absolute session lifetime (seconds)"), "86400");
    changeControlValue(getFormFieldControl(tree, "Workspace retention (days)"), "14");
    changeControlValue(getFormFieldControl(tree, "Warm pool mode"), "low-latency");

    tree = harness.render();
    submitForm(getSubmitForm(tree, "Create agent session"));
    await harness.flushMutations();

    expect(harness.createSession).toHaveBeenCalledTimes(1);
    expect(harness.createSession.mock.calls[0]?.[1]).toMatchObject({
      title: "Pinned lifecycle session",
      visibility: "private",
      provider: "codex",
      executionTargetId: "target-1",
      resourceLifecyclePolicy: {
        waitingKeepAliveSeconds: 600,
        suspendAfterIdleSeconds: null,
        absoluteSessionLifetimeSeconds: 86_400,
        workspaceRetentionDays: 14,
        warmPoolMode: "low-latency",
      },
    });
  });

  it("does not submit when session lifecycle validation fails", async () => {
    const harness = await createSubmitPathHarness();

    let tree = harness.render();
    changeControlValue(getFormFieldControl(tree, "Title"), "Rejected lifecycle session");
    clickControl(getClickableElement(tree, "Customize for this Session"));

    tree = harness.render();
    changeControlValue(getFormFieldControl(tree, "Waiting keep-alive (seconds)"), "59");

    tree = harness.render();
    submitForm(getSubmitForm(tree, "Create agent session"));
    await harness.flushMutations();

    expect(harness.getProjectProviderCapabilities).not.toHaveBeenCalled();
    expect(harness.createSession).not.toHaveBeenCalled();
  });
});
