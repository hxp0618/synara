// FILE: PlatformAdminShell.tsx
// Purpose: Keep Platform authority and customer-context state visible around every Admin workflow.
// Layer: Admin application shell

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  resolveControlPlaneInternalStatusBoardURL,
  type ControlPlanePlatformTenantOverview,
  type ControlPlaneSessionState,
} from "@synara/control-plane-client";
import {
  IconBuilding,
  IconAlertTriangle,
  IconCertificate,
  IconDeviceDesktop,
  IconExternalLink,
  IconHome,
  IconKey,
  IconLogout,
  IconRocket,
  IconActivityHeartbeat,
  IconSpeakerphone,
  IconChecklist,
  IconChartHistogram,
  IconRestore,
  IconShieldLock,
  IconUserShield,
} from "@tabler/icons-react";
import { useState } from "react";

import { SynaraLogo } from "../components/SynaraLogo";
import { Button, StatusPill } from "../components/ui";
import type { AdminDestination } from "./navigation";
import { canManagePlatform, platformQueryKeys } from "./platformQueries";
import { PlatformEntitlementManagement } from "./PlatformSubscriptionManagement";
import { PlatformDesktopAccess } from "./PlatformDesktopAccess";
import { PlatformSupportAccess } from "./PlatformSupportAccess";
import { PlatformReleaseGovernance } from "./PlatformReleaseGovernance";
import { PlatformIncidentGovernance } from "./PlatformIncidentGovernance";
import { PlatformIncidentExerciseGovernance } from "./PlatformIncidentExerciseGovernance";
import { PlatformOperationsExerciseGovernance } from "./PlatformOperationsExerciseGovernance";
import { PlatformInternalCostGovernance } from "./PlatformInternalCostGovernance";
import { PlatformSLOGovernance } from "./PlatformSLOGovernance";
import { PlatformRecoveryGovernance } from "./PlatformRecoveryGovernance";
import { PlatformPenetrationGovernance } from "./PlatformPenetrationGovernance";
import { PlatformCapacityGovernance } from "./PlatformCapacityGovernance";
import { PlatformComplianceGovernance } from "./PlatformComplianceGovernance";
import { PlatformProviderCommercialGovernance } from "./PlatformProviderCommercialGovernance";
import { PlatformGovernanceAuthorities } from "./PlatformGovernanceAuthorities";
import { PlatformTenantOperations } from "./PlatformTenantOperations";
import { PlatformTenantProvisioning } from "./PlatformTenantProvisioning";

const navigation = [
  { id: "tenants", label: "Overview", icon: IconHome, manageOnly: false },
  { id: "tenants", label: "Tenants", icon: IconBuilding, manageOnly: false },
  { id: "entitlements", label: "Entitlements", icon: IconChecklist, manageOnly: true },
  { id: "desktop", label: "Desktop access", icon: IconDeviceDesktop, manageOnly: true },
  { id: "support", label: "Support Access", icon: IconUserShield, manageOnly: false },
  { id: "authorities", label: "Governance roles", icon: IconKey, manageOnly: false },
  { id: "releases", label: "Release governance", icon: IconRocket, manageOnly: false },
  { id: "incidents", label: "Incidents", icon: IconAlertTriangle, manageOnly: false },
  {
    id: "incident-exercises",
    label: "Incident exercises",
    icon: IconSpeakerphone,
    manageOnly: false,
  },
  {
    id: "operations-exercises",
    label: "Operations exercises",
    icon: IconChecklist,
    manageOnly: false,
  },
  {
    id: "internal-cost-reviews",
    label: "Usage & cost reviews",
    icon: IconChartHistogram,
    manageOnly: false,
  },
  { id: "slo", label: "SLO windows", icon: IconActivityHeartbeat, manageOnly: false },
  { id: "recovery", label: "Recovery drills", icon: IconRestore, manageOnly: false },
  { id: "penetration", label: "Penetration reviews", icon: IconShieldLock, manageOnly: false },
  { id: "capacity", label: "Capacity reviews", icon: IconChartHistogram, manageOnly: false },
  { id: "compliance", label: "Compliance", icon: IconCertificate, manageOnly: false },
  { id: "providers", label: "Provider use", icon: IconShieldLock, manageOnly: false },
] as const;

export function PlatformAdminShell(props: {
  readonly session: ControlPlaneSessionState;
  readonly overview: ControlPlanePlatformTenantOverview;
}) {
  const queryClient = useQueryClient();
  const canManage = canManagePlatform(props.overview.operatorRole);
  const [destination, setDestination] = useState<AdminDestination>("tenants");
  const platformProfile = useQuery({
    queryKey: platformQueryKeys.profile,
    queryFn: controlPlaneClient.getPlatformProfile,
  });
  const internalStatusBoardURL = resolveControlPlaneInternalStatusBoardURL(platformProfile.data);
  const logout = useMutation({
    mutationFn: controlPlaneClient.logout,
    onSuccess: () => {
      queryClient.clear();
      window.location.reload();
    },
  });
  const activeDestination = canManage
    ? destination
    : destination === "entitlements" || destination === "provision" || destination === "desktop"
      ? "tenants"
      : destination;

  return (
    <div className="admin-shell">
      <aside className="admin-sidebar">
        <div className="brand-lockup">
          <SynaraLogo aria-label="Synara" />
          <div>
            <strong>Synara</strong>
            <span>Platform Admin</span>
          </div>
        </div>
        <nav aria-label="Platform Admin">
          {navigation
            .filter((item) => !item.manageOnly || canManage)
            .map((item, index) => {
              const Icon = item.icon;
              const selected =
                activeDestination === item.id && (item.id !== "tenants" || index === 0);
              return (
                <button
                  aria-current={selected ? "page" : undefined}
                  className="nav-item"
                  data-selected={selected ? "true" : undefined}
                  key={`${item.id}-${item.label}`}
                  onClick={() => setDestination(item.id)}
                >
                  <Icon aria-hidden size={18} />
                  <span>{item.label}</span>
                </button>
              );
            })}
          {internalStatusBoardURL ? (
            <a className="nav-item" href={internalStatusBoardURL} rel="noreferrer" target="_blank">
              <IconExternalLink aria-hidden size={18} />
              <span>Internal status</span>
            </a>
          ) : null}
        </nav>
        <div className="operator-card">
          <span className="operator-avatar" aria-hidden>
            {props.session.user.displayName.slice(0, 2).toUpperCase()}
          </span>
          <div>
            <strong>{props.session.user.displayName}</strong>
            <span>{props.session.user.email}</span>
          </div>
          <Button
            aria-label="Sign out"
            disabled={logout.isPending}
            onClick={() => logout.mutate()}
            size="xs"
            variant="ghost"
          >
            <IconLogout aria-hidden size={17} />
          </Button>
        </div>
      </aside>

      <div className="admin-workspace">
        <header className="authority-strip">
          <div>
            <IconShieldLock aria-hidden size={18} />
            <span>Platform authority</span>
            <StatusPill value={props.overview.operatorRole} tone="active" />
          </div>
          <span>
            Tenant context: <strong>none</strong>
          </span>
        </header>
        <main>
          {activeDestination === "tenants" ? (
            <PlatformTenantOperations overview={props.overview} onNavigate={setDestination} />
          ) : activeDestination === "provision" ? (
            <PlatformTenantProvisioning onNavigate={setDestination} />
          ) : activeDestination === "entitlements" ? (
            <PlatformEntitlementManagement overview={props.overview} onNavigate={setDestination} />
          ) : activeDestination === "desktop" ? (
            <PlatformDesktopAccess
              overview={props.overview}
              session={props.session}
              onNavigate={setDestination}
            />
          ) : activeDestination === "support" ? (
            <PlatformSupportAccess
              overview={props.overview}
              session={props.session}
              onNavigate={setDestination}
            />
          ) : activeDestination === "releases" ? (
            <PlatformReleaseGovernance canManage={canManage} />
          ) : activeDestination === "incidents" ? (
            <PlatformIncidentGovernance session={props.session} />
          ) : activeDestination === "incident-exercises" ? (
            <PlatformIncidentExerciseGovernance canManage={canManage} />
          ) : activeDestination === "operations-exercises" ? (
            <PlatformOperationsExerciseGovernance canManage={canManage} />
          ) : activeDestination === "internal-cost-reviews" ? (
            <PlatformInternalCostGovernance canManage={canManage} />
          ) : activeDestination === "slo" ? (
            <PlatformSLOGovernance canManage={canManage} />
          ) : activeDestination === "recovery" ? (
            <PlatformRecoveryGovernance canManage={canManage} />
          ) : activeDestination === "penetration" ? (
            <PlatformPenetrationGovernance canManage={canManage} />
          ) : activeDestination === "capacity" ? (
            <PlatformCapacityGovernance canManage={canManage} />
          ) : activeDestination === "compliance" ? (
            <PlatformComplianceGovernance
              canManage={canManage}
              currentUserId={props.session.user.userId}
            />
          ) : activeDestination === "providers" ? (
            <PlatformProviderCommercialGovernance canManage={canManage} />
          ) : (
            <PlatformGovernanceAuthorities
              canManage={props.overview.operatorRole === "owner"}
              currentUserId={props.session.user.userId}
            />
          )}
        </main>
      </div>
    </div>
  );
}
