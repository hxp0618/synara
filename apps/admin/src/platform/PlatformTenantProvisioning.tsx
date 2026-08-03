// FILE: PlatformTenantProvisioning.tsx
// Purpose: Provision audited internal Tenants without granting Platform membership.
// Layer: Admin Platform feature

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { controlPlaneClient } from "@synara/control-plane-client";
import { useState } from "react";

import { Button, Field, InlineError, Input, Select } from "../components/ui";
import { entitlementProfileLabel } from "./entitlementDisplay";
import type { AdminDestination } from "./navigation";
import { platformQueryKeys } from "./platformQueries";

export function PlatformTenantProvisioning(props: {
  readonly onNavigate: (destination: AdminDestination) => void;
}) {
  const queryClient = useQueryClient();
  const [ownerEmail, setOwnerEmail] = useState("");
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [region, setRegion] = useState("default");
  const [profileCode, setProfileCode] = useState<"standard" | "enterprise">("enterprise");
  const [status, setStatus] = useState<"active" | "evaluation">("active");
  const [evaluationExpiresAt, setEvaluationExpiresAt] = useState("");
  const provision = useMutation({
    mutationFn: () =>
      controlPlaneClient.provisionPlatformTenant({
        ownerEmail,
        name,
        slug,
        region,
        entitlementProfileCode: profileCode,
        status,
        ...(status === "evaluation"
          ? { evaluationExpiresAt: new Date(evaluationExpiresAt).toISOString() }
          : {}),
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: platformQueryKeys.tenants,
      });
      props.onNavigate("tenants");
    },
  });

  return (
    <div className="page-stack page-stack--narrow">
      <header className="page-heading">
        <div>
          <p className="page-heading__back">
            <button onClick={() => props.onNavigate("tenants")}>Tenant operations</button> /
            Provision
          </p>
          <h1>Provision internal Tenant</h1>
          <p>
            Creates an audited Tenant and root Organization for an existing active internal user
            account.
          </p>
        </div>
      </header>
      <section className="form-panel">
        <div className="boundary-callout">
          The initial Tenant Owner receives membership. The Platform Admin does not.
        </div>
        <form
          className="form-grid"
          onSubmit={(event) => {
            event.preventDefault();
            provision.mutate();
          }}
        >
          <Field label="Initial Owner email">
            <Input
              autoComplete="email"
              onChange={(event) => setOwnerEmail(event.target.value)}
              placeholder="owner@internal.example"
              required
              type="email"
              value={ownerEmail}
            />
          </Field>
          <Field label="Tenant name">
            <Input
              autoComplete="organization"
              onChange={(event) => setName(event.target.value)}
              placeholder="Internal Tenant name"
              required
              value={name}
            />
          </Field>
          <Field label="Tenant slug">
            <Input
              autoCapitalize="none"
              autoComplete="off"
              onChange={(event) => setSlug(event.target.value.toLowerCase())}
              pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"
              placeholder="internal-tenant"
              required
              value={slug}
            />
          </Field>
          <Field label="Region">
            <Input
              autoCapitalize="none"
              autoComplete="off"
              onChange={(event) => setRegion(event.target.value.toLowerCase())}
              placeholder="Region code"
              required
              value={region}
            />
          </Field>
          <Field label="Entitlement profile">
            <Select
              onChange={(event) => setProfileCode(event.target.value as "standard" | "enterprise")}
              value={profileCode}
            >
              <option value="enterprise">{entitlementProfileLabel("enterprise")}</option>
              <option value="standard">{entitlementProfileLabel("standard")}</option>
            </Select>
          </Field>
          <Field label="Initial entitlement status">
            <Select
              onChange={(event) => setStatus(event.target.value as "active" | "evaluation")}
              value={status}
            >
              <option value="active">Active</option>
              <option value="evaluation">Evaluation</option>
            </Select>
          </Field>
          {status === "evaluation" ? (
            <Field label="Evaluation expiry">
              <Input
                min={new Date().toISOString().slice(0, 16)}
                onChange={(event) => setEvaluationExpiresAt(event.target.value)}
                required
                type="datetime-local"
                value={evaluationExpiresAt}
              />
            </Field>
          ) : null}
          <div className="form-actions form-grid__wide">
            <Button disabled={provision.isPending} type="submit" variant="primary">
              {provision.isPending ? "Provisioning…" : "Provision Tenant"}
            </Button>
            <Button onClick={() => props.onNavigate("tenants")}>Cancel</Button>
          </div>
          <InlineError error={provision.error} />
        </form>
      </section>
    </div>
  );
}
