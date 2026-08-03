// FILE: PlatformSubscriptionManagement.tsx
// Purpose: Manage versioned internal entitlement profiles through audited Platform routes.
// Layer: Admin Platform feature

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  controlPlaneClient,
  type ControlPlanePlatformTenantOverview,
} from "@synara/control-plane-client";
import { useEffect, useState } from "react";

import { Button, Field, InlineError, Input, Select, StatusPill } from "../components/ui";
import { entitlementProfileLabel, tenantLifecycleLabel } from "./entitlementDisplay";
import type { AdminDestination } from "./navigation";
import { platformQueryKeys } from "./platformQueries";

function toDateTimeLocal(value: string): string {
  const date = new Date(value);
  const offsetMilliseconds = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offsetMilliseconds).toISOString().slice(0, 16);
}

export function PlatformEntitlementManagement(props: {
  readonly overview: ControlPlanePlatformTenantOverview;
  readonly onNavigate: (destination: AdminDestination) => void;
}) {
  const queryClient = useQueryClient();
  const [tenantId, setTenantId] = useState(props.overview.items[0]?.id ?? "");
  const [profileCode, setProfileCode] = useState<"standard" | "enterprise">("enterprise");
  const [status, setStatus] = useState<"active" | "evaluation">("active");
  const [periodStart, setPeriodStart] = useState("");
  const [periodEnd, setPeriodEnd] = useState("");
  const [evaluationEndsAt, setEvaluationEndsAt] = useState("");
  const [reason, setReason] = useState("");
  const entitlements = useQuery({
    queryKey: platformQueryKeys.entitlements(tenantId),
    queryFn: () => controlPlaneClient.getPlatformTenantEntitlements(tenantId),
    enabled: tenantId.length > 0,
  });

  useEffect(() => {
    if (!entitlements.data) return;
    setProfileCode(entitlements.data.profile.code === "standard" ? "standard" : "enterprise");
    setStatus(
      entitlements.data.profileAssignment.status === "evaluation" ? "evaluation" : "active",
    );
    setPeriodStart(toDateTimeLocal(entitlements.data.profileAssignment.reportingPeriodStart));
    setPeriodEnd(toDateTimeLocal(entitlements.data.profileAssignment.reportingPeriodEnd));
    setEvaluationEndsAt(
      entitlements.data.profileAssignment.evaluationEndsAt
        ? toDateTimeLocal(entitlements.data.profileAssignment.evaluationEndsAt)
        : "",
    );
  }, [entitlements.data]);

  const assign = useMutation({
    mutationFn: () => {
      if (!entitlements.data) throw new Error("Entitlement details are not loaded.");
      return controlPlaneClient.assignPlatformTenantEntitlementProfile(tenantId, {
        entitlementProfileCode: profileCode,
        status,
        expectedVersion: entitlements.data.profileAssignment.version,
        reportingPeriodStart: new Date(periodStart).toISOString(),
        reportingPeriodEnd: new Date(periodEnd).toISOString(),
        ...(status === "evaluation"
          ? { evaluationEndsAt: new Date(evaluationEndsAt).toISOString() }
          : {}),
        reason,
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(platformQueryKeys.entitlements(tenantId), snapshot);
      setReason("");
      void queryClient.invalidateQueries({
        queryKey: platformQueryKeys.tenants,
      });
    },
  });
  const selectedTenant = props.overview.items.find((tenant) => tenant.id === tenantId) ?? null;

  return (
    <div className="page-stack page-stack--narrow">
      <header className="page-heading">
        <div>
          <p className="page-heading__back">
            <button onClick={() => props.onNavigate("tenants")}>Tenant operations</button> /
            Entitlements
          </p>
          <h1>Manage internal entitlement profile</h1>
          <p>
            Assigns capacity and feature limits for this self-hosted installation; it is an internal
            control only.
          </p>
        </div>
      </header>
      <section className="form-panel">
        <form
          className="form-grid"
          onSubmit={(event) => {
            event.preventDefault();
            assign.mutate();
          }}
        >
          <Field label="Tenant">
            <Select onChange={(event) => setTenantId(event.target.value)} value={tenantId}>
              {props.overview.items.map((tenant) => (
                <option key={tenant.id} value={tenant.id}>
                  {tenant.name} · {entitlementProfileLabel(tenant.entitlementProfileCode)}
                </option>
              ))}
            </Select>
          </Field>
          <div className="field field--summary">
            <span className="field__label">Tenant lifecycle</span>
            {selectedTenant ? (
              <StatusPill
                value={tenantLifecycleLabel(selectedTenant.status)}
                tone={selectedTenant.status === "active" ? "active" : "warning"}
              />
            ) : null}
          </div>
          <Field label="Entitlement profile">
            <Select
              disabled={!entitlements.data}
              onChange={(event) => setProfileCode(event.target.value as "standard" | "enterprise")}
              value={profileCode}
            >
              <option value="enterprise">Enterprise</option>
              <option value="standard">Standard</option>
            </Select>
          </Field>
          <Field label="Entitlement status">
            <Select
              disabled={!entitlements.data}
              onChange={(event) => setStatus(event.target.value as "active" | "evaluation")}
              value={status}
            >
              <option value="active">Active</option>
              <option value="evaluation">Evaluation</option>
            </Select>
          </Field>
          <Field label="Period start">
            <Input
              disabled={!entitlements.data}
              onChange={(event) => setPeriodStart(event.target.value)}
              required
              type="datetime-local"
              value={periodStart}
            />
          </Field>
          <Field label="Period end">
            <Input
              disabled={!entitlements.data}
              min={periodStart}
              onChange={(event) => setPeriodEnd(event.target.value)}
              required
              type="datetime-local"
              value={periodEnd}
            />
          </Field>
          {status === "evaluation" ? (
            <Field label="Evaluation end">
              <Input
                disabled={!entitlements.data}
                min={periodStart}
                onChange={(event) => setEvaluationEndsAt(event.target.value)}
                required
                type="datetime-local"
                value={evaluationEndsAt}
              />
            </Field>
          ) : null}
          <Field label="Approval reference">
            <Input
              disabled={!entitlements.data}
              minLength={3}
              onChange={(event) => setReason(event.target.value)}
              placeholder="Required audit reason"
              required
              value={reason}
            />
          </Field>
          <div className="boundary-callout form-grid__wide">
            This is an internal entitlement assignment for usage limits and feature access. It does
            not create an external settlement or provider transaction.
          </div>
          <div className="form-actions form-grid__wide">
            <Button
              disabled={!entitlements.data || assign.isPending}
              type="submit"
              variant="primary"
            >
              {assign.isPending ? "Saving…" : "Save entitlement"}
            </Button>
            <Button onClick={() => props.onNavigate("tenants")}>Back</Button>
          </div>
          <div className="form-grid__wide">
            <InlineError error={entitlements.error ?? assign.error} />
          </div>
        </form>
      </section>
    </div>
  );
}
