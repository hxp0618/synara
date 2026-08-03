// FILE: TenantStatusLifecycleSettingsSection.tsx
// Purpose: Expose the audited Tenant status/deletion state machine to Tenant owners.
// Layer: Settings UI component
// Exports: TenantStatusLifecycleSettingsSection, tenantLifecycleTargets

import { useMutation } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { controlPlaneClient, type ControlPlaneTenantAccess } from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

type MutableTenantStatus = "active" | "suspended" | "closed";

const TENANT_LIFECYCLE_TARGETS: Readonly<
  Record<ControlPlaneTenantAccess["status"], ReadonlyArray<MutableTenantStatus>>
> = {
  evaluation: ["active", "suspended", "closed"],
  active: ["suspended", "closed"],
  suspended: ["active", "closed"],
  closed: ["active"],
};

export function tenantLifecycleTargets(
  status: ControlPlaneTenantAccess["status"],
): ReadonlyArray<MutableTenantStatus> {
  return TENANT_LIFECYCLE_TARGETS[status];
}

export function tenantLifecycleDisplayLabel(status: ControlPlaneTenantAccess["status"]): string {
  return status;
}

function lifecycleTimestamp(tenant: ControlPlaneTenantAccess): string | null {
  const value =
    tenant.status === "evaluation"
      ? tenant.evaluationExpiresAt
      : tenant.status === "suspended"
        ? tenant.suspendedAt
        : tenant.status === "closed"
          ? tenant.closedAt
          : null;
  if (!value) return null;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const label =
    tenant.status === "evaluation"
      ? "evaluation expires"
      : `${tenantLifecycleDisplayLabel(tenant.status)} since`;
  return `${label} ${parsed.toLocaleString()}`;
}

export function TenantStatusLifecycleSettingsSection(props: {
  tenant: ControlPlaneTenantAccess;
  onChanged: () => Promise<void>;
}) {
  const {
    Button,
    InlineError,
    Input,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    confirmAction,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const targets = tenantLifecycleTargets(props.tenant.status);
  const [target, setTarget] = useState<MutableTenantStatus>(targets[0] ?? "active");
  const [reason, setReason] = useState("");
  const [deletionReason, setDeletionReason] = useState("");
  const selectedTarget = targets.includes(target) ? target : targets[0];
  const version = props.tenant.lifecycleVersion;
  const versionReady = Number.isSafeInteger(version) && (version ?? 0) > 0;
  const isOwner = props.tenant.role === "owner";
  const timestamp = useMemo(() => lifecycleTimestamp(props.tenant), [props.tenant]);

  const transition = useMutation({
    mutationFn: async () => {
      if (!versionReady || version === undefined || selectedTarget === undefined) {
        throw new Error("Reload the Tenant lifecycle before changing its status.");
      }
      return controlPlaneClient.transitionTenant(props.tenant.id, {
        toStatus: selectedTarget,
        expectedVersion: version,
        reason: reason.trim(),
      });
    },
    onSuccess: async () => {
      setReason("");
      await props.onChanged();
    },
  });
  const requestDeletion = useMutation({
    mutationFn: async () => {
      if (!versionReady || version === undefined) {
        throw new Error("Reload the Tenant lifecycle before requesting deletion.");
      }
      if (
        !confirmAction(
          `Request deletion for ${props.tenant.name}? Active work is stopped and the Tenant leaves the normal Tenant list.`,
        )
      ) {
        return false;
      }
      await controlPlaneClient.requestTenantDeletion(props.tenant.id, {
        expectedVersion: version,
        reason: deletionReason.trim(),
      });
      return true;
    },
    onSuccess: async (requested) => {
      if (!requested) return;
      setDeletionReason("");
      await props.onChanged();
    },
  });

  return (
    <SettingsSection title="Tenant lifecycle">
      <SettingsListRow
        title="Audited status authority"
        description={[
          versionReady ? `lifecycle version ${version}` : "lifecycle version unavailable",
          timestamp,
          "Status changes require an owner reason and reject stale versions.",
        ]
          .filter(Boolean)
          .join(" · ")}
        actions={
          <StatusPill
            value={tenantLifecycleDisplayLabel(props.tenant.status)}
            active={props.tenant.status === "active"}
          />
        }
      />
      {isOwner ? (
        <SettingsRow
          title="Change Tenant status"
          description="Suspending or closing is blocked until active Executions are drained. Reactivation preserves the audit lineage."
        >
          <form
            className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto]"
            onSubmit={(event) => {
              event.preventDefault();
              transition.mutate();
            }}
          >
            <select
              aria-label="Tenant lifecycle target"
              className={nativeSelectClassName}
              disabled={!versionReady || transition.isPending}
              value={selectedTarget}
              onChange={(event) => setTarget(event.target.value as MutableTenantStatus)}
            >
              {targets.map((item) => (
                <option key={item} value={item}>
                  {item}
                </option>
              ))}
            </select>
            <Input
              aria-label="Tenant lifecycle reason"
              minLength={10}
              maxLength={1000}
              placeholder="Reason recorded in Tenant Audit"
              required
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
            <Button
              disabled={
                !versionReady ||
                selectedTarget === undefined ||
                reason.trim().length < 10 ||
                transition.isPending
              }
              size="sm"
              type="submit"
            >
              {transition.isPending ? "Changing…" : `Change to ${selectedTarget ?? "status"}`}
            </Button>
          </form>
          <InlineError error={transition.error} />
        </SettingsRow>
      ) : (
        <SettingsListRow
          title="Owner approval required"
          description="Tenant administrators can inspect lifecycle policy, but only an Owner can change Tenant status."
        />
      )}
      {isOwner && props.tenant.status === "closed" ? (
        <SettingsRow
          title="Request Tenant deletion"
          description="Deletion is a separate audited step. Legal Hold blocks it; the recovery inventory remains available to an Owner while cleanup is reversible."
        >
          <form
            className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto]"
            onSubmit={(event) => {
              event.preventDefault();
              requestDeletion.mutate();
            }}
          >
            <Input
              aria-label="Tenant deletion reason"
              minLength={10}
              maxLength={1000}
              placeholder="Deletion reason recorded in Tenant Audit"
              required
              value={deletionReason}
              onChange={(event) => setDeletionReason(event.target.value)}
            />
            <Button
              disabled={
                !versionReady || deletionReason.trim().length < 10 || requestDeletion.isPending
              }
              size="sm"
              type="submit"
              variant="destructive"
            >
              {requestDeletion.isPending ? "Requesting…" : "Request deletion"}
            </Button>
          </form>
          <InlineError error={requestDeletion.error} />
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}
