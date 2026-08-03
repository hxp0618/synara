// FILE: TenantMemberLifecycleControls.tsx
// Purpose: Provide audited role, suspension, Session revocation, and removal controls for Tenant members.
// Layer: Settings UI component
// Exports: TenantMemberLifecycleControls

import { useMutation } from "@tanstack/react-query";
import { useState } from "react";

import {
  controlPlaneClient,
  type ControlPlaneTenantAccess,
  type ControlPlaneTenantMember,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

const TENANT_ROLES = [
  "member",
  "auditor",
  "cost_admin",
  "security_admin",
  "admin",
  "owner",
] as const;

type MemberAction =
  | { kind: "role"; role: string }
  | { kind: "status"; status: "active" | "suspended" }
  | { kind: "revoke-sessions" }
  | { kind: "remove" };

export function TenantMemberLifecycleControls(props: {
  tenantId: string;
  actorUserId: string;
  actorRole: ControlPlaneTenantAccess["role"];
  member: ControlPlaneTenantMember;
  onChanged: () => Promise<void>;
}) {
  const { Button, InlineError, confirmAction, nativeSelectClassName } = useEnterpriseUiHost();
  const [role, setRole] = useState(props.member.role);
  const isSelf = props.member.userId === props.actorUserId;
  const canManageOwner = props.actorRole === "owner" || props.member.role !== "owner";
  const action = useMutation({
    mutationFn: async (input: MemberAction) => {
      switch (input.kind) {
        case "role":
          return controlPlaneClient.updateTenantMember(props.tenantId, props.member.userId, {
            role: input.role,
          });
        case "status":
          return controlPlaneClient.updateTenantMember(props.tenantId, props.member.userId, {
            status: input.status,
          });
        case "revoke-sessions":
          return controlPlaneClient.revokeTenantUserSessions(props.tenantId, props.member.userId);
        case "remove":
          return controlPlaneClient.removeTenantMember(props.tenantId, props.member.userId);
      }
    },
    onSuccess: async () => props.onChanged(),
  });

  if (isSelf) {
    return (
      <span className="text-[10px] text-muted-foreground">
        Another administrator must change your access.
      </span>
    );
  }
  if (!canManageOwner) {
    return (
      <span className="text-[10px] text-muted-foreground">
        Only a Tenant Owner can manage another Owner.
      </span>
    );
  }

  const suspendOrReactivate = () => {
    const nextStatus = props.member.status === "active" ? "suspended" : "active";
    if (
      nextStatus === "suspended" &&
      !confirmAction(
        `Suspend ${props.member.displayName || props.member.email}? Their Tenant Sessions and user Credentials will be revoked.`,
      )
    ) {
      return;
    }
    action.mutate({ kind: "status", status: nextStatus });
  };

  return (
    <div className="flex max-w-full flex-wrap items-center justify-end gap-1.5">
      <select
        aria-label={`Tenant role for ${props.member.displayName || props.member.email}`}
        className={`${nativeSelectClassName} min-w-28 py-1 text-[11px]`}
        disabled={action.isPending}
        value={role}
        onChange={(event) => setRole(event.target.value)}
      >
        {TENANT_ROLES.filter((item) => item !== "owner" || props.actorRole === "owner").map(
          (item) => (
            <option key={item} value={item}>
              {item}
            </option>
          ),
        )}
      </select>
      <Button
        disabled={action.isPending || role === props.member.role}
        size="xs"
        type="button"
        variant="outline"
        onClick={() => action.mutate({ kind: "role", role })}
      >
        Save role
      </Button>
      <Button
        disabled={action.isPending}
        size="xs"
        type="button"
        variant="outline"
        onClick={suspendOrReactivate}
      >
        {props.member.status === "active" ? "Suspend" : "Reactivate"}
      </Button>
      <Button
        disabled={action.isPending}
        size="xs"
        type="button"
        variant="outline"
        onClick={() => {
          if (
            confirmAction(
              `Revoke every active ${props.member.displayName || props.member.email} Login Session for this Tenant?`,
            )
          ) {
            action.mutate({ kind: "revoke-sessions" });
          }
        }}
      >
        Revoke Sessions
      </Button>
      {props.member.status === "suspended" ? (
        <Button
          className="text-destructive hover:text-destructive"
          disabled={action.isPending}
          size="xs"
          type="button"
          variant="outline"
          onClick={() => {
            if (
              confirmAction(
                `Remove ${props.member.displayName || props.member.email} from this Tenant? Immutable dependencies may require keeping the suspended Membership.`,
              )
            ) {
              action.mutate({ kind: "remove" });
            }
          }}
        >
          Remove
        </Button>
      ) : null}
      <InlineError error={action.error} />
    </div>
  );
}
