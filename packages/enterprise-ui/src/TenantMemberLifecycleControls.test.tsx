import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { TenantMemberLifecycleControls } from "./TenantMemberLifecycleControls";
import type {
  ControlPlaneTenantAccess,
  ControlPlaneTenantMember,
} from "@synara/control-plane-client";
import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

function member(overrides: Partial<ControlPlaneTenantMember> = {}): ControlPlaneTenantMember {
  return {
    tenantId: "tenant-1",
    userId: "member-1",
    email: "member@example.com",
    displayName: "Member",
    role: "member",
    status: "active",
    joinedAt: "2026-07-30T00:00:00Z",
    createdAt: "2026-07-30T00:00:00Z",
    updatedAt: "2026-07-30T00:00:00Z",
    ...overrides,
  };
}

function renderControls(
  value: ControlPlaneTenantMember,
  actorRole: ControlPlaneTenantAccess["role"] = "owner",
  actorUserId = "owner-1",
): string {
  const queryClient = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <TenantMemberLifecycleControls
          actorRole={actorRole}
          actorUserId={actorUserId}
          member={value}
          tenantId="tenant-1"
          onChanged={async () => undefined}
        />
      </EnterpriseUiHostProvider>
    </QueryClientProvider>,
  );
}

describe("TenantMemberLifecycleControls", () => {
  it("offers role, suspension, and Session revocation for an active member", () => {
    const markup = renderControls(member());
    expect(markup).toContain("Save role");
    expect(markup).toContain("Suspend");
    expect(markup).toContain("Revoke Sessions");
    expect(markup).not.toContain(">Remove<");
  });

  it("makes removal a suspended-member operation and documents reactivation separately", () => {
    const markup = renderControls(member({ status: "suspended" }));
    expect(markup).toContain("Reactivate");
    expect(markup).toContain(">Remove<");
  });

  it("prevents self-management and keeps Owner management owner-only", () => {
    expect(renderControls(member(), "owner", "member-1")).toContain(
      "Another administrator must change your access",
    );
    expect(renderControls(member({ role: "owner" }), "admin")).toContain(
      "Only a Tenant Owner can manage another Owner",
    );
  });
});
