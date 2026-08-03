// FILE: TenantLegalHoldsSettingsSection.tsx
// Purpose: Manage audited, scoped Legal Holds that gate retention and Tenant deletion.
// Layer: Settings UI component

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { controlPlaneClient, type ControlPlaneLegalHold } from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

const scopeTypes: ReadonlyArray<ControlPlaneLegalHold["scopeType"]> = [
  "tenant",
  "user",
  "organization",
  "project",
  "session",
];

export function legalHoldsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "legal-holds"] as const;
}

export function TenantLegalHoldsSettingsSection(props: { tenantId: string; canManage: boolean }) {
  const { InlineError, SettingsListRow, SettingsSection, StatusPill } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const holds = useQuery({
    queryKey: legalHoldsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listLegalHolds(props.tenantId),
    retry: false,
  });

  if (holds.isPending) {
    return (
      <SettingsSection title="Legal Holds">
        <SettingsListRow title="Loading Legal Holds…" />
      </SettingsSection>
    );
  }

  const items = holds.data?.items ?? [];
  const active = items.filter((hold) => hold.status === "active");
  return (
    <SettingsSection title="Legal Holds">
      <SettingsListRow
        title="Retention and deletion gate"
        description="An active hold preserves matching Sessions, Workspaces, Artifacts, and Checkpoints, and blocks deletion of the Tenant. Releases are one-way and audited."
        actions={<StatusPill value={`${active.length} active`} active={active.length > 0} />}
      />
      {items.length === 0 ? (
        <SettingsListRow title="No Legal Holds" />
      ) : (
        items.map((hold) => (
          <SettingsListRow
            key={hold.id}
            title={`${hold.name} · ${hold.matterReference}`}
            description={`${hold.scopeType} ${hold.scopeId} · ${hold.reason}`}
            actions={<StatusPill value={hold.status} active={hold.status === "active"} />}
          />
        ))
      )}
      {props.canManage ? (
        <>
          <CreateLegalHoldForm
            tenantId={props.tenantId}
            onSaved={() =>
              void queryClient.invalidateQueries({ queryKey: legalHoldsQueryKey(props.tenantId) })
            }
          />
          {active.length > 0 ? (
            <ReleaseLegalHoldForm
              tenantId={props.tenantId}
              holds={active}
              onSaved={() =>
                void queryClient.invalidateQueries({ queryKey: legalHoldsQueryKey(props.tenantId) })
              }
            />
          ) : null}
        </>
      ) : null}
      <InlineError error={holds.error} />
    </SettingsSection>
  );
}

function CreateLegalHoldForm(props: { tenantId: string; onSaved: () => void }) {
  const {
    Button,
    FormField,
    InlineError,
    Input,
    SettingsRow,
    formGridClassName,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const [scopeType, setScopeType] = useState<ControlPlaneLegalHold["scopeType"]>("tenant");
  const [scopeId, setScopeId] = useState("");
  const [name, setName] = useState("");
  const [matterReference, setMatterReference] = useState("");
  const [reason, setReason] = useState("");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createLegalHold(props.tenantId, {
        scopeType,
        scopeId: scopeType === "tenant" ? props.tenantId : scopeId.trim(),
        name,
        matterReference,
        reason,
      }),
    onSuccess: () => {
      setName("");
      setMatterReference("");
      setReason("");
      props.onSaved();
    },
  });

  return (
    <SettingsRow
      title="Place Legal Hold"
      description="Use a case or matter reference. Scope UUIDs are validated against this Tenant; secrets and customer content do not belong in the reason."
    >
      <form
        className={formGridClassName}
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <FormField label="Scope">
          <select
            className={nativeSelectClassName}
            value={scopeType}
            onChange={(event) =>
              setScopeType(event.target.value as ControlPlaneLegalHold["scopeType"])
            }
          >
            {scopeTypes.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </FormField>
        <FormField label="Scope UUID">
          <Input
            disabled={scopeType === "tenant"}
            placeholder={scopeType === "tenant" ? props.tenantId : "Resource UUID"}
            required={scopeType !== "tenant"}
            value={scopeId}
            onChange={(event) => setScopeId(event.target.value)}
          />
        </FormField>
        <FormField label="Hold name">
          <Input
            maxLength={160}
            required
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </FormField>
        <FormField label="Matter reference">
          <Input
            maxLength={160}
            required
            value={matterReference}
            onChange={(event) => setMatterReference(event.target.value)}
          />
        </FormField>
        <div className="sm:col-span-2">
          <Input
            minLength={10}
            maxLength={2000}
            placeholder="Auditable reason"
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
        <div className="sm:col-span-2">
          <Button disabled={create.isPending} size="sm" type="submit">
            {create.isPending ? "Placing hold…" : "Place Legal Hold"}
          </Button>
          <InlineError error={create.error} />
        </div>
      </form>
    </SettingsRow>
  );
}

function ReleaseLegalHoldForm(props: {
  tenantId: string;
  holds: ReadonlyArray<ControlPlaneLegalHold>;
  onSaved: () => void;
}) {
  const { Button, InlineError, Input, SettingsRow, formGridClassName, nativeSelectClassName } =
    useEnterpriseUiHost();
  const [holdId, setHoldId] = useState(props.holds[0]?.id ?? "");
  const [reason, setReason] = useState("");
  const release = useMutation({
    mutationFn: () => {
      const hold = props.holds.find((item) => item.id === holdId);
      if (!hold) throw new Error("Select an active Legal Hold.");
      return controlPlaneClient.releaseLegalHold(props.tenantId, hold.id, {
        expectedVersion: hold.version,
        reason,
      });
    },
    onSuccess: () => {
      setReason("");
      props.onSaved();
    },
  });

  return (
    <SettingsRow
      title="Release Legal Hold"
      description="Release is permanent. Confirm the controlling legal or compliance matter is closed before continuing."
    >
      <form
        className={formGridClassName}
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          release.mutate();
        }}
      >
        <select
          aria-label="Active Legal Hold"
          className={nativeSelectClassName}
          value={holdId}
          onChange={(event) => setHoldId(event.target.value)}
        >
          {props.holds.map((hold) => (
            <option key={hold.id} value={hold.id}>
              {hold.name} · {hold.matterReference}
            </option>
          ))}
        </select>
        <Input
          minLength={10}
          maxLength={2000}
          placeholder="Release reason"
          required
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        <div className="sm:col-span-2">
          <Button disabled={release.isPending} size="sm" type="submit" variant="destructive">
            {release.isPending ? "Releasing…" : "Release Legal Hold"}
          </Button>
          <InlineError error={release.error} />
        </div>
      </form>
    </SettingsRow>
  );
}
