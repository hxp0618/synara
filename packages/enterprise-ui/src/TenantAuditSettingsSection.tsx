import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  controlPlaneClient,
  resolveAuditLogExportUrl,
  type ControlPlaneAuditLogFilters,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

const AUDIT_PAGE_SIZE = 25;
const auditTimeFormatter = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "medium",
});

export function auditLogQueryKey(
  tenantId: string,
  filters: ControlPlaneAuditLogFilters,
  cursor?: string,
) {
  return [
    "control-plane",
    "tenants",
    tenantId,
    "audit-logs",
    filters.action ?? "",
    filters.actorType ?? "",
    filters.resourceType ?? "",
    cursor ?? "",
  ] as const;
}

export function TenantAuditSettingsSection(props: { tenantId: string }) {
  const {
    Button,
    FormField,
    InlineError,
    Input,
    LinkButton,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    formGridClassName,
    nativeSelectClassName,
  } = useEnterpriseUiHost();
  const [action, setAction] = useState("");
  const [actorType, setActorType] = useState<ControlPlaneAuditLogFilters["actorType"]>("");
  const [resourceType, setResourceType] = useState("");
  const [filters, setFilters] = useState<ControlPlaneAuditLogFilters>({});
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorHistory, setCursorHistory] = useState<ReadonlyArray<string | undefined>>([]);
  const auditLogs = useQuery({
    queryKey: auditLogQueryKey(props.tenantId, filters, cursor),
    queryFn: () =>
      controlPlaneClient.listAuditLogs(props.tenantId, filters, {
        limit: AUDIT_PAGE_SIZE,
        ...(cursor === undefined ? {} : { cursor }),
      }),
    retry: false,
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setCursor(undefined);
    setCursorHistory([]);
    setFilters({
      ...(action.trim() === "" ? {} : { action: action.trim() }),
      ...(actorType ? { actorType } : {}),
      ...(resourceType.trim() === "" ? {} : { resourceType: resourceType.trim() }),
    });
  };
  const reset = () => {
    setAction("");
    setActorType("");
    setResourceType("");
    setFilters({});
    setCursor(undefined);
    setCursorHistory([]);
  };

  return (
    <SettingsSection title="Audit log">
      <SettingsRow
        title="Search and export"
        description="Filter immutable tenant audit events, or download the complete filtered result without buffering it in the browser."
      >
        <form className={formGridClassName} onSubmit={submit}>
          <FormField label="Action">
            <Input
              placeholder="session.created"
              value={action}
              onChange={(event) => setAction(event.target.value)}
            />
          </FormField>
          <FormField label="Resource type">
            <Input
              placeholder="agent_session"
              value={resourceType}
              onChange={(event) => setResourceType(event.target.value)}
            />
          </FormField>
          <FormField label="Actor type">
            <select
              className={nativeSelectClassName}
              value={actorType}
              onChange={(event) =>
                setActorType(event.target.value as ControlPlaneAuditLogFilters["actorType"])
              }
            >
              <option value="">All actors</option>
              <option value="user">User</option>
              <option value="worker">Worker</option>
              <option value="service_account">Service account</option>
              <option value="system">System</option>
            </select>
          </FormField>
          <div className="flex flex-wrap items-end gap-2">
            <Button size="sm" type="submit">
              Search audit log
            </Button>
            <Button size="sm" variant="outline" onClick={reset}>
              Clear
            </Button>
          </div>
          <div className="flex flex-wrap gap-2 sm:col-span-2">
            <LinkButton
              href={resolveAuditLogExportUrl(props.tenantId, "jsonl", filters)}
              size="sm"
              variant="outline"
            >
              Download JSONL
            </LinkButton>
            <LinkButton
              href={resolveAuditLogExportUrl(props.tenantId, "csv", filters)}
              size="sm"
              variant="outline"
            >
              Download CSV
            </LinkButton>
          </div>
          <div className="sm:col-span-2">
            <InlineError error={auditLogs.error} />
          </div>
        </form>
      </SettingsRow>

      {auditLogs.isPending ? <SettingsListRow title="Loading audit events…" /> : null}
      {auditLogs.data?.items.map((entry) => (
        <SettingsListRow
          key={entry.eventId}
          title={entry.action}
          description={`${entry.resourceType} · ${auditTimeFormatter.format(new Date(entry.occurredAt))} · request ${entry.requestId}`}
          actions={<StatusPill value={entry.actorType} active={false} />}
        />
      ))}
      {auditLogs.data?.items.length === 0 ? (
        <SettingsListRow
          title="No matching audit events"
          description="Adjust the filters or clear them to view recent tenant activity."
        />
      ) : null}
      {auditLogs.data ? (
        <SettingsListRow
          title={`Page ${cursorHistory.length + 1}`}
          description="Events are ordered newest first with a stable cursor."
          actions={
            <span className="flex gap-2">
              <Button
                disabled={cursorHistory.length === 0 || auditLogs.isFetching}
                size="sm"
                variant="outline"
                onClick={() => {
                  const previous = cursorHistory.at(-1);
                  setCursor(previous);
                  setCursorHistory((current) => current.slice(0, -1));
                }}
              >
                Previous
              </Button>
              <Button
                disabled={auditLogs.data.nextCursor === null || auditLogs.isFetching}
                size="sm"
                variant="outline"
                onClick={() => {
                  setCursorHistory((current) => [...current, cursor]);
                  setCursor(auditLogs.data.nextCursor ?? undefined);
                }}
              >
                Next
              </Button>
            </span>
          }
        />
      ) : null}
    </SettingsSection>
  );
}
