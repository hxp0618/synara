import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_DISPLAY_NAMES,
  type ProviderCapabilityId,
  type ProviderCapabilityProjection,
  type ProviderCapabilityProjectionReasonCode,
  type ProviderCapabilityProjectionStatus,
  type ProviderCapabilityProjectionSupportMode,
  type ProviderInteractionMode,
  type ProviderKind,
} from "@synara/contracts";

export type ControlPlaneCapabilityDecisionStatus =
  | "local"
  | "loading"
  | "error"
  | ProviderCapabilityProjectionStatus;

export type ControlPlaneCapabilityDecision = {
  provider: ProviderKind;
  capabilityId: ProviderCapabilityId;
  allowed: boolean;
  temporary: boolean;
  status: ControlPlaneCapabilityDecisionStatus;
  reasonCode: ProviderCapabilityProjectionReasonCode | null;
  supportMode: ProviderCapabilityProjectionSupportMode | null;
  message: string | null;
};

export type ControlPlaneDispatchDecision = {
  allowed: boolean;
  temporary: boolean;
  decisions: ReadonlyArray<ControlPlaneCapabilityDecision>;
  blockingDecision: ControlPlaneCapabilityDecision | null;
  message: string | null;
};

const UNOBSERVED_QUEUEABLE_CAPABILITIES = new Set<ProviderCapabilityId>([
  "start-session",
  "send-turn",
  "plan-mode",
  "interrupt-turn",
]);

const catalogByProvider = new Map(
  PROVIDER_CAPABILITY_CATALOG.providers.map((entry) => [entry.provider, entry] as const),
);

function capabilityLabel(capabilityId: ProviderCapabilityId): string {
  return capabilityId.replaceAll("-", " ");
}

function capabilityMessage(input: {
  provider: ProviderKind;
  capabilityId: ProviderCapabilityId;
  status: ControlPlaneCapabilityDecisionStatus;
  reasonCode: ProviderCapabilityProjectionReasonCode | null;
  allowed: boolean;
}): string | null {
  if (input.status === "local" || (input.allowed && input.status === "supported")) {
    return null;
  }
  const provider = PROVIDER_DISPLAY_NAMES[input.provider];
  const capability = capabilityLabel(input.capabilityId);
  if (input.status === "loading") {
    return `Checking whether ${provider} can use ${capability} on this SaaS target.`;
  }
  if (input.status === "error") {
    return `Could not verify whether ${provider} can use ${capability} on this SaaS target.`;
  }
  if (input.status === "unobserved" || input.reasonCode === "worker_manifest_required") {
    return input.allowed
      ? `No compatible Worker manifest is observed yet. ${provider} can be queued while the target is waiting for a Worker.`
      : `No compatible Worker manifest is observed yet, so ${provider} cannot use ${capability} right now.`;
  }
  switch (input.reasonCode) {
    case "provider_not_installed":
      return `${provider} is not installed on this SaaS target.`;
    case "provider_version_incompatible":
      return `${provider} is installed, but its runtime version is incompatible with this SaaS target.`;
    case "execution_target_unavailable":
      return "The selected SaaS execution target is unavailable.";
    default:
      return `${provider} does not support ${capability} on this SaaS target.`;
  }
}

function staticCapabilityUnsupported(
  provider: ProviderKind,
  capabilityId: ProviderCapabilityId,
): boolean {
  const catalog = catalogByProvider.get(provider as Exclude<ProviderKind, "droid">);
  return (
    !catalog ||
    catalog.supportTier === "local-only" ||
    catalog.capabilities[capabilityId] === "unsupported"
  );
}

export function resolveControlPlaneCapabilityDecision(input: {
  isAuthoritative: boolean;
  projection: ProviderCapabilityProjection | null | undefined;
  projectionError?: unknown;
  provider: ProviderKind;
  capabilityId: ProviderCapabilityId;
}): ControlPlaneCapabilityDecision {
  if (!input.isAuthoritative) {
    return {
      provider: input.provider,
      capabilityId: input.capabilityId,
      allowed: true,
      temporary: false,
      status: "local",
      reasonCode: null,
      supportMode: null,
      message: null,
    };
  }

  if (staticCapabilityUnsupported(input.provider, input.capabilityId)) {
    const decision = {
      provider: input.provider,
      capabilityId: input.capabilityId,
      allowed: false,
      temporary: false,
      status: "unsupported" as const,
      reasonCode: "capability_unsupported" as const,
      supportMode: null,
    };
    return { ...decision, message: capabilityMessage(decision) };
  }

  if (input.projectionError) {
    const decision = {
      provider: input.provider,
      capabilityId: input.capabilityId,
      allowed: false,
      temporary: true,
      status: "error" as const,
      reasonCode: null,
      supportMode: null,
    };
    return { ...decision, message: capabilityMessage(decision) };
  }

  if (input.projection === undefined) {
    const decision = {
      provider: input.provider,
      capabilityId: input.capabilityId,
      allowed: false,
      temporary: true,
      status: "loading" as const,
      reasonCode: null,
      supportMode: null,
    };
    return { ...decision, message: capabilityMessage(decision) };
  }

  const item = input.projection?.items.find(
    (candidate) =>
      candidate.provider === input.provider && candidate.capabilityId === input.capabilityId,
  );
  if (!item) {
    const allowed = UNOBSERVED_QUEUEABLE_CAPABILITIES.has(input.capabilityId);
    const decision = {
      provider: input.provider,
      capabilityId: input.capabilityId,
      allowed,
      temporary: true,
      status: "unobserved" as const,
      reasonCode: "worker_manifest_required" as const,
      supportMode: null,
    };
    return { ...decision, message: capabilityMessage(decision) };
  }

  const allowed =
    item.status === "supported" ||
    (item.status === "unobserved" && UNOBSERVED_QUEUEABLE_CAPABILITIES.has(input.capabilityId));
  const decision = {
    provider: input.provider,
    capabilityId: input.capabilityId,
    allowed,
    temporary: item.status === "unobserved",
    status: item.status,
    reasonCode: item.reasonCode,
    supportMode: item.supportMode ?? null,
  };
  return { ...decision, message: capabilityMessage(decision) };
}

export function resolveControlPlaneTurnDispatchDecision(input: {
  isAuthoritative: boolean;
  projection: ProviderCapabilityProjection | null | undefined;
  projectionError?: unknown;
  provider: ProviderKind;
  includeSessionStart: boolean;
  interactionMode: ProviderInteractionMode;
}): ControlPlaneDispatchDecision {
  const capabilityIds: ProviderCapabilityId[] = [
    ...(input.includeSessionStart ? (["start-session"] as const) : []),
    "send-turn",
    ...(input.interactionMode === "plan" ? (["plan-mode"] as const) : []),
  ];
  const decisions = capabilityIds.map((capabilityId) =>
    resolveControlPlaneCapabilityDecision({
      isAuthoritative: input.isAuthoritative,
      projection: input.projection,
      ...(input.projectionError ? { projectionError: input.projectionError } : {}),
      provider: input.provider,
      capabilityId,
    }),
  );
  const blockingDecision = decisions.find((decision) => !decision.allowed) ?? null;
  const temporaryDecision = decisions.find((decision) => decision.temporary) ?? null;
  return {
    allowed: blockingDecision === null,
    temporary: temporaryDecision !== null,
    decisions,
    blockingDecision,
    message: blockingDecision?.message ?? temporaryDecision?.message ?? null,
  };
}

export function assertControlPlaneCapabilityAllowed(
  decision: ControlPlaneCapabilityDecision | ControlPlaneDispatchDecision,
): void {
  if (decision.allowed) return;
  throw new Error(decision.message ?? "This action is unavailable on the selected SaaS target.");
}

export function providerCanStartSaaSSession(input: {
  projection: ProviderCapabilityProjection | null | undefined;
  projectionError?: unknown;
  provider: ProviderKind;
}): ControlPlaneDispatchDecision {
  return resolveControlPlaneTurnDispatchDecision({
    isAuthoritative: true,
    projection: input.projection,
    ...(input.projectionError ? { projectionError: input.projectionError } : {}),
    provider: input.provider,
    includeSessionStart: true,
    interactionMode: "default",
  });
}
