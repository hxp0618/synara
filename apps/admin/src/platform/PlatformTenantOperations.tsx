// FILE: PlatformTenantOperations.tsx
// Purpose: Provide the searchable cross-Tenant triage entrypoint for Platform operators.
// Layer: Admin Platform feature

import { useQuery } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlanePlatformTenantOverview,
} from "@synara/control-plane-client";
import { IconInfoCircle, IconSearch } from "@tabler/icons-react";
import { useMemo, useState } from "react";

import { Button, EmptyState, InlineError, Input, Select, StatusPill } from "../components/ui";
import { entitlementProfileLabel, tenantLifecycleLabel } from "./entitlementDisplay";
import type { AdminDestination } from "./navigation";
import { canManagePlatform, platformQueryKeys } from "./platformQueries";
import { platformTenantOperationsLines } from "./platformOperationsDisplay";

type PlatformTenant = ControlPlanePlatformTenantOverview["items"][number];

function lifecycleTone(status: PlatformTenant["status"]) {
  if (status === "active") return "active" as const;
  if (status === "evaluation" || status === "suspended") return "warning" as const;
  if (status === "closed" || status === "deleting") return "danger" as const;
  return "neutral" as const;
}

export function PlatformTenantOperations(props: {
  readonly overview: ControlPlanePlatformTenantOverview;
  readonly onNavigate: (destination: AdminDestination) => void;
}) {
  const canManage = canManagePlatform(props.overview.operatorRole);
  const [query, setQuery] = useState("");
  const [entitlementProfile, setEntitlementProfile] = useState("all");
  const [region, setRegion] = useState("all");
  const [lifecycle, setLifecycle] = useState("all");
  const [selectedTenantId, setSelectedTenantId] = useState(props.overview.items[0]?.id ?? "");
  const grants = useQuery({
    queryKey: platformQueryKeys.supportAccess,
    queryFn: controlPlaneClient.listPlatformSupportAccess,
    refetchInterval: 30_000,
  });
  const entitlements = useQuery({
    queryKey: platformQueryKeys.entitlements(selectedTenantId),
    queryFn: () => controlPlaneClient.getPlatformTenantEntitlements(selectedTenantId),
    enabled: canManage && selectedTenantId.length > 0,
  });

  const entitlementProfiles = useMemo(
    () => [...new Set(props.overview.items.map((tenant) => tenant.entitlementProfileCode))].sort(),
    [props.overview.items],
  );
  const regions = useMemo(
    () => [...new Set(props.overview.items.map((tenant) => tenant.region))].sort(),
    [props.overview.items],
  );
  const filtered = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return props.overview.items.filter(
      (tenant) =>
        (normalized.length === 0 ||
          tenant.name.toLowerCase().includes(normalized) ||
          tenant.slug.toLowerCase().includes(normalized)) &&
        (entitlementProfile === "all" || tenant.entitlementProfileCode === entitlementProfile) &&
        (region === "all" || tenant.region === region) &&
        (lifecycle === "all" || tenant.status === lifecycle),
    );
  }, [entitlementProfile, lifecycle, props.overview.items, query, region]);
  const selected =
    props.overview.items.find((tenant) => tenant.id === selectedTenantId) ?? filtered[0] ?? null;
  const tenantGrants = grants.data?.items.filter((grant) => grant.tenantId === selected?.id) ?? [];
  const activeGrant = tenantGrants.find((grant) => grant.status === "active") ?? null;

  return (
    <div className="page-stack">
      <header className="page-heading">
        <div>
          <h1>Tenant operations</h1>
          <p>Provision, triage, and support internal Tenants without Tenant membership.</p>
          <p className="authority-note">
            <IconInfoCircle aria-hidden size={16} /> Platform roles do not grant Tenant membership.
          </p>
        </div>
        {canManage ? (
          <Button variant="primary" onClick={() => props.onNavigate("provision")}>
            Provision Tenant
          </Button>
        ) : null}
      </header>

      <section aria-label="Tenant filters" className="filter-bar">
        <label className="search-control">
          <IconSearch aria-hidden size={17} />
          <span className="sr-only">Search Tenants</span>
          <Input
            aria-label="Search Tenants"
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search Tenants…"
            value={query}
          />
        </label>
        <Select
          aria-label="Filter by entitlement profile"
          onChange={(event) => setEntitlementProfile(event.target.value)}
          value={entitlementProfile}
        >
          <option value="all">All entitlement profiles</option>
          {entitlementProfiles.map((value) => (
            <option key={value} value={value}>
              {entitlementProfileLabel(value)}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by Region"
          onChange={(event) => setRegion(event.target.value)}
          value={region}
        >
          <option value="all">All Regions</option>
          {regions.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by lifecycle"
          onChange={(event) => setLifecycle(event.target.value)}
          value={lifecycle}
        >
          <option value="all">All lifecycles</option>
          <option value="active">Active</option>
          <option value="evaluation">Evaluation</option>
          <option value="suspended">Suspended</option>
          <option value="closed">Closed</option>
          <option value="deleting">Deleting</option>
        </Select>
      </section>

      <section className="tenant-table" aria-label="Internal Tenants">
        <div className="tenant-table__header" aria-hidden>
          <span>Tenant</span>
          <span>Entitlement profile</span>
          <span>Region</span>
          <span>Lifecycle</span>
          <span>Operations</span>
        </div>
        {filtered.length === 0 ? (
          <EmptyState
            title="No matching Tenants"
            description="Clear one or more filters and try again."
          />
        ) : (
          filtered.map((tenant) => (
            <div
              className="tenant-table__row"
              data-selected={tenant.id === selected?.id ? "true" : undefined}
              key={tenant.id}
            >
              <div data-label="Tenant">
                <strong>{tenant.name}</strong>
                <small>{tenant.slug}</small>
              </div>
              <div data-label="Entitlement profile">
                {entitlementProfileLabel(tenant.entitlementProfileCode)}
              </div>
              <div data-label="Region">{tenant.region}</div>
              <div data-label="Lifecycle">
                <StatusPill
                  value={tenantLifecycleLabel(tenant.status)}
                  tone={lifecycleTone(tenant.status)}
                />
              </div>
              <div data-label="Operations">
                <Button size="sm" onClick={() => setSelectedTenantId(tenant.id)}>
                  Open
                </Button>
              </div>
            </div>
          ))
        )}
      </section>

      {selected ? (
        <section className="tenant-detail" aria-labelledby="selected-tenant-heading">
          <header className="tenant-detail__header">
            <div>
              <h2 id="selected-tenant-heading">{selected.name}</h2>
              <p>{selected.slug}</p>
            </div>
            <StatusPill
              value={tenantLifecycleLabel(selected.status)}
              tone={lifecycleTone(selected.status)}
            />
          </header>
          <div className="tenant-detail__body">
            <div className="tenant-detail__triage">
              <h3>Operational snapshot</h3>
              {platformTenantOperationsLines(selected, props.overview.generatedAt).map((line) => (
                <p key={line}>{line}</p>
              ))}
              <small>Captured {new Date(props.overview.generatedAt).toLocaleString()}.</small>
            </div>
            <div className="tenant-detail__entitlement">
              <h3>Entitlement profile</h3>
              {canManage ? (
                entitlements.isPending ? (
                  <p>Loading entitlement…</p>
                ) : entitlements.data ? (
                  <>
                    <p>
                      {entitlementProfileLabel(entitlements.data.profile.code)} ·{" "}
                      {tenantLifecycleLabel(entitlements.data.profileAssignment.status)} · version{" "}
                      {entitlements.data.profileAssignment.version}
                    </p>
                    <Button size="sm" onClick={() => props.onNavigate("entitlements")}>
                      Manage
                    </Button>
                  </>
                ) : (
                  <InlineError error={entitlements.error} />
                )
              ) : (
                <p>Platform Admin authority is required.</p>
              )}
            </div>
            <div className="tenant-detail__support">
              <h3>Support Access</h3>
              {grants.isPending ? (
                <p>Loading requests…</p>
              ) : (
                <>
                  <p>
                    {activeGrant
                      ? `Active until ${new Date(activeGrant.expiresAt!).toLocaleString()}`
                      : "No active grant"}
                  </p>
                  <Button size="sm" variant="primary" onClick={() => props.onNavigate("support")}>
                    Request access
                  </Button>
                  <Button size="sm" onClick={() => props.onNavigate("support")}>
                    Review requests
                  </Button>
                </>
              )}
              <InlineError error={grants.error} />
            </div>
            {canManage ? (
              <div className="tenant-detail__desktop">
                <h3>Desktop access</h3>
                <p>Issue a short-lived, single-use link for an already authorized subject.</p>
                <Button size="sm" onClick={() => props.onNavigate("desktop")}>
                  Manage devices
                </Button>
              </div>
            ) : null}
          </div>
        </section>
      ) : null}
    </div>
  );
}
