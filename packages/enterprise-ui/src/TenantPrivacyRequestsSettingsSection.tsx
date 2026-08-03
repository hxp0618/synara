// FILE: TenantPrivacyRequestsSettingsSection.tsx
// Purpose: Self-service and administrator workflow for access export and erasure DSARs.
// Layer: Settings UI component

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  controlPlaneClient,
  type ControlPlanePrivacyRequest,
  type ControlPlaneTenantMember,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

export function privacyRequestsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "privacy-requests"] as const;
}

export function TenantPrivacyRequestsSettingsSection(props: {
  tenantId: string;
  canManage: boolean;
  currentUserId: string;
  members: ReadonlyArray<ControlPlaneTenantMember>;
}) {
  const {
    Button,
    InlineError,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    downloadJsonFile,
  } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const requests = useQuery({
    queryKey: privacyRequestsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listPrivacyRequests(props.tenantId),
    retry: false,
    refetchInterval: 30_000,
  });
  const tenantExport = useMutation({
    mutationFn: () => controlPlaneClient.executeTenantDataExport(props.tenantId),
    onSuccess: (result) =>
      downloadJsonFile(`synara-tenant-export-${result.receipt.id}.json`, result.bundle),
  });
  const currentUserId = props.currentUserId;
  if (requests.isPending) {
    return (
      <SettingsSection title="Privacy requests">
        <SettingsListRow title="Loading Privacy Requests…" />
      </SettingsSection>
    );
  }

  const items = requests.data?.items ?? [];
  const actionable = items.filter((item) =>
    ["requested", "verified", "approved", "failed"].includes(item.status),
  );
  const refresh = () =>
    void queryClient.invalidateQueries({ queryKey: privacyRequestsQueryKey(props.tenantId) });
  return (
    <SettingsSection title="Privacy requests">
      <SettingsListRow
        title="DSAR workflow"
        description="Access exports and erasure requests have a 30-day due date, immutable transition history, administrator verification, and Legal Hold checks."
      />
      {props.canManage ? (
        <SettingsRow
          title="Tenant data export"
          description="Generate a point-in-time JSON bundle of Tenant content and governance records. Credential and SSO secrets are excluded; artifact payloads remain in object storage."
        >
          <Button
            disabled={tenantExport.isPending}
            size="sm"
            type="button"
            onClick={() => tenantExport.mutate()}
          >
            {tenantExport.isPending ? "Generating…" : "Generate & download Tenant export"}
          </Button>
          <InlineError error={tenantExport.error} />
        </SettingsRow>
      ) : null}
      {items.length === 0 ? (
        <SettingsListRow title="No Privacy Requests" />
      ) : (
        items.map((item) => (
          <SettingsListRow
            key={item.id}
            title={`${item.requestType === "access_export" ? "Access export" : "Erasure"} · ${subjectLabel(item.subjectUserId, props.members, currentUserId)}`}
            description={`Due ${new Date(item.dueAt).toLocaleDateString()} · v${item.version} · ${item.lastTransitionReason}`}
            actions={<StatusPill value={item.status} active={item.status === "completed"} />}
          />
        ))
      )}
      <CreatePrivacyRequestForm
        canManage={props.canManage}
        currentUserId={currentUserId}
        members={props.members}
        onSaved={refresh}
        tenantId={props.tenantId}
      />
      {actionable.length > 0 ? (
        <PrivacyRequestActionForm
          canManage={props.canManage}
          currentUserId={currentUserId}
          onSaved={refresh}
          requests={actionable}
          tenantId={props.tenantId}
        />
      ) : null}
      <InlineError error={requests.error} />
    </SettingsSection>
  );
}

function CreatePrivacyRequestForm(props: {
  tenantId: string;
  currentUserId: string;
  canManage: boolean;
  members: ReadonlyArray<ControlPlaneTenantMember>;
  onSaved: () => void;
}) {
  const {
    Button,
    FormField,
    InlineError,
    Input,
    SettingsRow,
    formGridClassName,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const [requestType, setRequestType] = useState<"access_export" | "erasure">("access_export");
  const [subjectUserId, setSubjectUserId] = useState(props.currentUserId);
  const [reason, setReason] = useState("");
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createPrivacyRequest(props.tenantId, {
        ...(props.canManage && subjectUserId !== props.currentUserId ? { subjectUserId } : {}),
        requestType,
        reason,
      }),
    onSuccess: () => {
      setReason("");
      props.onSaved();
    },
  });

  return (
    <SettingsRow
      title="Submit Privacy Request"
      description="The subject can request their own export or erasure. Privacy administrators may submit on behalf of an active or suspended Tenant member."
    >
      <form
        className={formGridClassName}
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <FormField label="Request type">
          <select
            className={nativeSelectClassName}
            value={requestType}
            onChange={(event) => setRequestType(event.target.value as "access_export" | "erasure")}
          >
            <option value="access_export">Access export</option>
            <option value="erasure">Erasure</option>
          </select>
        </FormField>
        {props.canManage ? (
          <FormField label="Subject">
            <select
              className={nativeSelectClassName}
              value={subjectUserId}
              onChange={(event) => setSubjectUserId(event.target.value)}
            >
              {props.members.map((member) => (
                <option key={member.userId} value={member.userId}>
                  {member.displayName || member.email} · {member.status}
                </option>
              ))}
            </select>
          </FormField>
        ) : null}
        <div className="sm:col-span-2">
          <Input
            minLength={10}
            maxLength={2000}
            placeholder="Request reason"
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
        <div className="sm:col-span-2">
          <Button disabled={create.isPending || !props.currentUserId} size="sm" type="submit">
            {create.isPending ? "Submitting…" : "Submit Privacy Request"}
          </Button>
          <InlineError error={create.error} />
        </div>
      </form>
    </SettingsRow>
  );
}

function PrivacyRequestActionForm(props: {
  tenantId: string;
  currentUserId: string;
  canManage: boolean;
  requests: ReadonlyArray<ControlPlanePrivacyRequest>;
  onSaved: () => void;
}) {
  const {
    Button,
    InlineError,
    Input,
    SettingsRow,
    downloadJsonFile,
    formGridClassName,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const [requestId, setRequestId] = useState(props.requests[0]?.id ?? "");
  const [reason, setReason] = useState("");
  const selected = props.requests.find((item) => item.id === requestId) ?? null;
  const transition = useMutation({
    mutationFn: (toStatus: string) => {
      if (!selected) throw new Error("Select a Privacy Request.");
      return controlPlaneClient.transitionPrivacyRequest(props.tenantId, selected.id, {
        expectedVersion: selected.version,
        toStatus,
        reason,
      });
    },
    onSuccess: () => {
      setReason("");
      props.onSaved();
    },
  });
  const execute = useMutation({
    mutationFn: async () => {
      if (!selected) throw new Error("Select an approved Privacy Request.");
      if (selected.requestType === "access_export") {
        const result = await controlPlaneClient.executePrivacyExport(
          props.tenantId,
          selected.id,
          selected.version,
        );
        downloadJsonFile(`synara-privacy-export-${selected.id}.json`, result.bundle);
        return result.request;
      }
      const result = await controlPlaneClient.executePrivacyErasure(
        props.tenantId,
        selected.id,
        selected.version,
      );
      return result.request;
    },
    onSuccess: props.onSaved,
  });
  const canCancel =
    selected != null &&
    ["requested", "verified", "approved"].includes(selected.status) &&
    (props.canManage || selected.subjectUserId === props.currentUserId);

  return (
    <SettingsRow
      title="Review or execute Privacy Request"
      description="Verify identity before approval. Erasure is destructive and requires no active Legal Hold, subject Execution, or last-owner dependency."
    >
      <form className={formGridClassName} onSubmit={(event) => event.preventDefault()}>
        <select
          aria-label="Privacy Request"
          className={nativeSelectClassName}
          value={requestId}
          onChange={(event) => setRequestId(event.target.value)}
        >
          {props.requests.map((item) => (
            <option key={item.id} value={item.id}>
              {item.requestType} · {item.status} · {item.subjectUserId}
            </option>
          ))}
        </select>
        <Input
          minLength={10}
          maxLength={2000}
          placeholder="Transition reason"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        <div className="flex flex-wrap gap-2 sm:col-span-2">
          {props.canManage && selected?.status === "requested" ? (
            <Button
              disabled={transition.isPending || reason.trim().length < 10}
              size="sm"
              type="button"
              onClick={() => transition.mutate("verified")}
            >
              Verify identity
            </Button>
          ) : null}
          {props.canManage && (selected?.status === "verified" || selected?.status === "failed") ? (
            <Button
              disabled={transition.isPending || reason.trim().length < 10}
              size="sm"
              type="button"
              onClick={() => transition.mutate("approved")}
            >
              Approve
            </Button>
          ) : null}
          {props.canManage &&
          (selected?.status === "requested" ||
            selected?.status === "verified" ||
            selected?.status === "failed") ? (
            <Button
              disabled={transition.isPending || reason.trim().length < 10}
              size="sm"
              type="button"
              variant="outline"
              onClick={() => transition.mutate("denied")}
            >
              Deny
            </Button>
          ) : null}
          {canCancel ? (
            <Button
              disabled={transition.isPending || reason.trim().length < 10}
              size="sm"
              type="button"
              variant="outline"
              onClick={() => transition.mutate("cancelled")}
            >
              Cancel
            </Button>
          ) : null}
          {selected?.status === "approved" &&
          (selected.requestType === "access_export" || props.canManage) ? (
            <Button
              disabled={execute.isPending}
              size="sm"
              type="button"
              variant={selected.requestType === "erasure" ? "destructive" : "default"}
              onClick={() => execute.mutate()}
            >
              {execute.isPending
                ? "Processing…"
                : selected.requestType === "access_export"
                  ? "Generate & download export"
                  : "Execute erasure"}
            </Button>
          ) : null}
        </div>
      </form>
      <InlineError error={transition.error ?? execute.error} />
    </SettingsRow>
  );
}

function subjectLabel(
  subjectUserId: string,
  members: ReadonlyArray<ControlPlaneTenantMember>,
  currentUserId: string,
): string {
  if (subjectUserId === currentUserId) return "You";
  const member = members.find((item) => item.userId === subjectUserId);
  return member?.displayName || member?.email || subjectUserId;
}
