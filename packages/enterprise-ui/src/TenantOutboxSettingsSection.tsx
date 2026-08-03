// FILE: TenantOutboxSettingsSection.tsx
// Purpose: Expose redacted Tenant Outbox diagnostics and audited dead-letter replay without database access.
// Layer: Settings UI component
// Exports: TenantOutboxSettingsSection, tenantOutboxQueryKey

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { controlPlaneClient, type ControlPlaneOutboxMessage } from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

type OutboxStatus = "all" | "pending" | "retrying" | "dead-letter" | "published";

export function tenantOutboxQueryKey(tenantId: string, status: OutboxStatus) {
  return ["control-plane", "tenants", tenantId, "outbox", status] as const;
}

function formatTimestamp(value: string | null | undefined): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function outboxDescription(item: ControlPlaneOutboxMessage): string {
  const timestamps =
    item.status === "published"
      ? `published ${formatTimestamp(item.publishedAt)}`
      : item.status === "dead-letter"
        ? `dead-lettered ${formatTimestamp(item.deadLetteredAt)}`
        : `available ${formatTimestamp(item.availableAt)}`;
  const claimed = item.claimedAt ? ` · claimed ${formatTimestamp(item.claimedAt)}` : "";
  const error = item.lastError ? " · last error recorded (redacted from UI)" : "";
  return `${item.messageKey} · ${item.attempts} attempts · ${timestamps}${claimed}${error}`;
}

export function TenantOutboxSettingsSection(props: { tenantId: string; canManage: boolean }) {
  const {
    Button,
    InlineError,
    SettingsListRow,
    SettingsSection,
    StatusPill,
    confirmAction,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const [status, setStatus] = useState<OutboxStatus>("dead-letter");
  const messages = useQuery({
    queryKey: tenantOutboxQueryKey(props.tenantId, status),
    queryFn: () =>
      controlPlaneClient.listTenantOutboxMessages(props.tenantId, { status, limit: 100 }),
    retry: false,
    refetchInterval: 30_000,
  });
  const replay = useMutation({
    mutationFn: (item: ControlPlaneOutboxMessage) =>
      controlPlaneClient.replayTenantOutboxMessage(props.tenantId, item.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ["control-plane", "tenants", props.tenantId, "outbox"],
      });
    },
  });

  return (
    <SettingsSection title="Delivery Outbox">
      <SettingsListRow
        title="Redacted delivery diagnostics"
        description="Inspect topic, key, state, attempts and timestamps without exposing payload or headers. Dead-letter replay is audited and never edits the original message body."
        actions={
          <select
            aria-label="Outbox status"
            className={nativeSelectClassName}
            value={status}
            onChange={(event) => setStatus(event.target.value as OutboxStatus)}
          >
            <option value="dead-letter">Dead letter</option>
            <option value="retrying">Retrying</option>
            <option value="pending">Pending</option>
            <option value="published">Published</option>
            <option value="all">All</option>
          </select>
        }
      />
      {messages.isPending ? <SettingsListRow title="Loading Outbox messages…" /> : null}
      {messages.error ? (
        <SettingsListRow
          title="Could not load Outbox messages"
          description={messages.error.message}
          actions={
            <Button size="sm" variant="outline" onClick={() => void messages.refetch()}>
              Retry
            </Button>
          }
        />
      ) : null}
      {messages.data?.items.map((item) => (
        <SettingsListRow
          key={item.id}
          title={item.topic}
          description={outboxDescription(item)}
          actions={
            <div className="flex flex-wrap items-center justify-end gap-1.5">
              <StatusPill value={item.status} active={item.status === "published"} />
              {props.canManage && item.status === "dead-letter" ? (
                <Button
                  disabled={replay.isPending}
                  size="xs"
                  type="button"
                  variant="outline"
                  onClick={() => {
                    if (
                      confirmAction(
                        `Replay dead-letter ${item.topic} / ${item.messageKey}? Confirm the downstream failure is fixed first.`,
                      )
                    ) {
                      replay.mutate(item);
                    }
                  }}
                >
                  Replay
                </Button>
              ) : null}
            </div>
          }
        />
      ))}
      {messages.data?.items.length === 0 ? (
        <SettingsListRow
          title={`No ${status === "all" ? "" : `${status} `}Outbox messages`}
          description="Change the filter to inspect another delivery state."
        />
      ) : null}
      <InlineError error={replay.error} />
    </SettingsSection>
  );
}
