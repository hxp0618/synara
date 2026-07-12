import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import {
  controlPlaneClient,
  resolveAuditLogExportUrl,
  type ControlPlaneAuditLogFilters,
} from "~/lib/controlPlaneClient";

const AUDIT_PAGE_SIZE = 25;
const auditTimeFormatter = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "medium",
});

export function TenantAuditSettingsSection(props: { tenantId: string }) {
  const [action, setAction] = useState("");
  const [actorType, setActorType] = useState<ControlPlaneAuditLogFilters["actorType"]>("");
  const [resourceType, setResourceType] = useState("");
  const [filters, setFilters] = useState<ControlPlaneAuditLogFilters>({});
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorHistory, setCursorHistory] = useState<ReadonlyArray<string | undefined>>([]);
  const auditLogs = useQuery({
    queryKey: [
      "control-plane",
      "tenants",
      props.tenantId,
      "audit-logs",
      filters.action ?? "",
      filters.actorType ?? "",
      filters.resourceType ?? "",
      cursor ?? "",
    ],
    queryFn: () =>
      controlPlaneClient.listAuditLogs(props.tenantId, filters, {
        limit: AUDIT_PAGE_SIZE,
        cursor,
      }),
    retry: false,
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setCursor(undefined);
    setCursorHistory([]);
    setFilters({ action, actorType, resourceType });
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
        <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submit}>
          <ControlPlaneFormField label="Action">
            <Input
              placeholder="session.created"
              value={action}
              onChange={(event) => setAction(event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Resource type">
            <Input
              placeholder="agent_session"
              value={resourceType}
              onChange={(event) => setResourceType(event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Actor type">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
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
          </ControlPlaneFormField>
          <div className="flex flex-wrap items-end gap-2">
            <Button size="sm" type="submit">
              Search audit log
            </Button>
            <Button size="sm" variant="outline" onClick={reset}>
              Clear
            </Button>
          </div>
          <div className="flex flex-wrap gap-2 sm:col-span-2">
            <Button
              render={<a href={resolveAuditLogExportUrl(props.tenantId, "jsonl", filters)} />}
              size="sm"
              variant="outline"
            >
              Download JSONL
            </Button>
            <Button
              render={<a href={resolveAuditLogExportUrl(props.tenantId, "csv", filters)} />}
              size="sm"
              variant="outline"
            >
              Download CSV
            </Button>
          </div>
          <div className="sm:col-span-2">
            <ControlPlaneInlineError error={auditLogs.error} />
          </div>
        </form>
      </SettingsRow>

      {auditLogs.isPending ? <SettingsListRow title="Loading audit events…" /> : null}
      {auditLogs.data?.items.map((entry) => (
        <SettingsListRow
          key={entry.eventId}
          title={entry.action}
          description={`${entry.resourceType} · ${auditTimeFormatter.format(new Date(entry.occurredAt))} · request ${entry.requestId}`}
          actions={<ControlPlaneStatusPill value={entry.actorType} active={false} />}
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
