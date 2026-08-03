// FILE: TenantDeletionRecoverySettingsSection.tsx
// Purpose: Keep deletion recovery reachable after a soft-deleted Tenant leaves normal session lists.
// Layer: Settings UI component
// Exports: TenantDeletionRecoverySettingsSection, tenantDeletionRequestsQueryKey

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { controlPlaneClient } from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

export const tenantDeletionRequestsQueryKey = [
  "control-plane",
  "tenants",
  "deletion-requests",
] as const;

export function TenantDeletionRecoverySettingsSection(props: { onRestored: () => Promise<void> }) {
  const {
    Button,
    InlineError,
    Input,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const requests = useQuery({
    queryKey: tenantDeletionRequestsQueryKey,
    queryFn: controlPlaneClient.listTenantDeletionRequests,
    retry: false,
  });
  const [selectedTenantId, setSelectedTenantId] = useState("");
  const [reason, setReason] = useState("");
  const selected =
    requests.data?.items.find((item) => item.id === selectedTenantId) ??
    requests.data?.items[0] ??
    null;
  const restore = useMutation({
    mutationFn: async () => {
      if (!selected) throw new Error("Select a Tenant deletion request.");
      return controlPlaneClient.restoreTenant(selected.id, {
        expectedVersion: selected.lifecycleVersion,
        reason: reason.trim(),
      });
    },
    onSuccess: async () => {
      setReason("");
      await queryClient.invalidateQueries({ queryKey: tenantDeletionRequestsQueryKey });
      await props.onRestored();
    },
  });

  if (requests.isPending) return null;
  if (requests.error) {
    return (
      <SettingsSection title="Tenant deletion recovery">
        <SettingsListRow
          title="Could not load deletion requests"
          description={requests.error.message}
          actions={
            <Button size="sm" variant="outline" onClick={() => void requests.refetch()}>
              Retry
            </Button>
          }
        />
      </SettingsSection>
    );
  }
  if (!requests.data || requests.data.items.length === 0) return null;

  return (
    <SettingsSection title="Tenant deletion recovery">
      {requests.data.items.map((item) => (
        <SettingsListRow
          key={item.id}
          title={item.name}
          description={`${item.slug} · requested ${new Date(item.deletionRequestedAt).toLocaleString()} · lifecycle version ${item.lifecycleVersion}`}
          actions={<StatusPill value="deleting" />}
        />
      ))}
      <SettingsRow
        title="Withdraw a deletion request"
        description="Recovery returns the Tenant to closed state. It fails closed while active work or Workspace cleanup is incomplete."
      >
        <form
          className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto]"
          onSubmit={(event) => {
            event.preventDefault();
            restore.mutate();
          }}
        >
          <select
            aria-label="Tenant deletion request"
            className={nativeSelectClassName}
            disabled={restore.isPending}
            value={selected?.id ?? ""}
            onChange={(event) => setSelectedTenantId(event.target.value)}
          >
            {requests.data.items.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
          <Input
            aria-label="Tenant recovery reason"
            minLength={10}
            maxLength={1000}
            placeholder="Recovery reason recorded in Tenant Audit"
            required
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
          <Button
            disabled={selected === null || reason.trim().length < 10 || restore.isPending}
            size="sm"
            type="submit"
          >
            {restore.isPending ? "Restoring…" : "Restore as closed"}
          </Button>
        </form>
        <InlineError error={restore.error} />
      </SettingsRow>
    </SettingsSection>
  );
}
