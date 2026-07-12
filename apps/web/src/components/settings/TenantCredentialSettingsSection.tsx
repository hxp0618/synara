import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";

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
import { Textarea } from "~/components/ui/textarea";
import {
  controlPlaneClient,
  type ControlPlaneOrganization,
  type ControlPlaneProviderCredential,
} from "~/lib/controlPlaneClient";
import { cn } from "~/lib/utils";

export function credentialsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "credentials"] as const;
}

function parsePayload(value: string): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new Error("Credential payload must be a valid JSON object.");
  }
  if (parsed === null || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error("Credential payload must be a JSON object.");
  }
  if (Object.keys(parsed).length === 0) {
    throw new Error("Credential payload must not be empty.");
  }
  return parsed as Record<string, unknown>;
}

function parseExpiry(value: string): string | null {
  if (value.trim() === "") return null;
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime()) || parsed.getTime() <= Date.now()) {
    throw new Error("Credential expiry must be in the future.");
  }
  return parsed.toISOString();
}

function credentialDescription(
  item: ControlPlaneProviderCredential,
  organizationNames: ReadonlyMap<string, string>,
): string {
  const scope = item.organizationId
    ? (organizationNames.get(item.organizationId) ?? `Organization ${item.organizationId.slice(0, 8)}`)
    : "All organizations";
  const expiry = item.expiresAt
    ? `expires ${new Date(item.expiresAt).toLocaleString()}`
    : "no expiry";
  return `${item.provider} · ${item.credentialType.replaceAll("_", " ")} · ${scope} · ${expiry}`;
}

export function TenantCredentialSettingsSection(props: {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
}) {
  const queryClient = useQueryClient();
  const credentials = useQuery({
    queryKey: credentialsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listCredentials(props.tenantId),
    retry: false,
  });
  const organizationNames = useMemo(
    () => new Map(props.organizations.map((item) => [item.id, item.name])),
    [props.organizations],
  );
  const [name, setName] = useState("");
  const [provider, setProvider] = useState("");
  const [credentialType, setCredentialType] = useState("api_key");
  const [organizationId, setOrganizationId] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [createPayload, setCreatePayload] = useState("");
  const [createPending, setCreatePending] = useState(false);
  const [createError, setCreateError] = useState<unknown>(null);
  const [rotateCredentialId, setRotateCredentialId] = useState("");
  const [rotateExpiresAt, setRotateExpiresAt] = useState("");
  const [rotatePayload, setRotatePayload] = useState("");
  const [rotatePending, setRotatePending] = useState(false);
  const [rotateError, setRotateError] = useState<unknown>(null);
  const [revokePendingId, setRevokePendingId] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<unknown>(null);

  const activeCredentials = credentials.data?.items.filter((item) => item.revokedAt === null) ?? [];
  const selectedRotation = activeCredentials.find((item) => item.id === rotateCredentialId) ?? null;

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setCreateError(null);
    setCreatePending(true);
    try {
      const payload = parsePayload(createPayload);
      const item = await controlPlaneClient.createCredential(props.tenantId, {
        organizationId: organizationId || undefined,
        name,
        provider,
        credentialType,
        payload,
        expiresAt: parseExpiry(expiresAt) ?? undefined,
      });
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneProviderCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({ items: [...(current?.items ?? []), item] }),
      );
      setName("");
      setProvider("");
      setExpiresAt("");
      setCreatePayload("");
    } catch (error) {
      setCreateError(error);
    } finally {
      setCreatePending(false);
    }
  };

  const rotate = async (event: FormEvent) => {
    event.preventDefault();
    setRotateError(null);
    if (!selectedRotation) {
      setRotateError(new Error("Select an active Credential to rotate."));
      return;
    }
    setRotatePending(true);
    try {
      const payload = parsePayload(rotatePayload);
      const item = await controlPlaneClient.rotateCredential(
        props.tenantId,
        selectedRotation.id,
        {
          expectedVersion: selectedRotation.version,
          payload,
          expiresAt: parseExpiry(rotateExpiresAt),
        },
      );
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneProviderCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({
          items: (current?.items ?? []).map((existing) =>
            existing.id === item.id ? item : existing,
          ),
        }),
      );
      setRotatePayload("");
      setRotateExpiresAt("");
    } catch (error) {
      setRotateError(error);
    } finally {
      setRotatePending(false);
    }
  };

  const revoke = async (item: ControlPlaneProviderCredential) => {
    if (!window.confirm(`Revoke ${item.name}? Running agents will no longer be able to resolve it.`)) {
      return;
    }
    setRevokeError(null);
    setRevokePendingId(item.id);
    try {
      await controlPlaneClient.revokeCredential(props.tenantId, item.id);
      await credentials.refetch();
      if (rotateCredentialId === item.id) {
        setRotateCredentialId("");
        setRotatePayload("");
      }
    } catch (error) {
      setRevokeError(error);
    } finally {
      setRevokePendingId(null);
    }
  };

  return (
    <SettingsSection title="Provider credentials">
      {credentials.isPending ? <SettingsListRow title="Loading Provider Credentials…" /> : null}
      {credentials.error ? (
        <SettingsListRow
          title="Could not load Provider Credentials"
          description={credentials.error.message}
          actions={
            <Button size="sm" variant="outline" onClick={() => void credentials.refetch()}>
              Retry
            </Button>
          }
        />
      ) : null}
      {credentials.data?.items.map((item) => (
        <SettingsListRow
          key={item.id}
          title={item.name}
          description={credentialDescription(item, organizationNames)}
          actions={
            <span className="flex flex-wrap items-center justify-end gap-1.5">
              <span className="text-[10px] tabular-nums text-muted-foreground">v{item.version}</span>
              <ControlPlaneStatusPill value={item.revokedAt ? "revoked" : "active"} />
              {item.revokedAt === null ? (
                <Button
                  className="text-destructive hover:text-destructive"
                  disabled={revokePendingId === item.id}
                  size="sm"
                  variant="outline"
                  onClick={() => void revoke(item)}
                >
                  {revokePendingId === item.id ? "Revoking…" : "Revoke"}
                </Button>
              ) : null}
            </span>
          }
        />
      ))}
      {credentials.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Provider Credentials"
          description="Add an encrypted tenant-wide or Organization-scoped Credential for agent execution."
        />
      ) : null}
      <SettingsRow
        title="Add encrypted Credential"
        description="The JSON payload is envelope-encrypted before storage and is never returned to the browser."
      >
        <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={(event) => void create(event)}>
          <ControlPlaneFormField label="Name">
            <Input required value={name} onChange={(event) => setName(event.target.value)} />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Provider">
            <Input
              autoCapitalize="none"
              placeholder="openai"
              required
              value={provider}
              onChange={(event) => setProvider(event.target.value.toLowerCase())}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Credential type">
            <Input
              autoCapitalize="none"
              placeholder="api_key"
              required
              value={credentialType}
              onChange={(event) => setCredentialType(event.target.value.toLowerCase())}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Scope">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={organizationId}
              onChange={(event) => setOrganizationId(event.target.value)}
            >
              <option value="">All organizations in this tenant</option>
              {props.organizations.map((organization) => (
                <option key={organization.id} value={organization.id}>
                  {organization.name}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Expiry (optional)">
            <Input type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            <ControlPlaneFormField label="Secret JSON payload">
              <Textarea
                autoComplete="off"
                placeholder={'{"apiKey":"…"}'}
                required
                spellCheck={false}
                value={createPayload}
                onChange={(event) => setCreatePayload(event.target.value)}
              />
            </ControlPlaneFormField>
          </div>
          <div className="sm:col-span-2">
            <Button disabled={createPending} size="sm" type="submit">
              {createPending ? "Encrypting…" : "Add Credential"}
            </Button>
            <ControlPlaneInlineError error={createError} />
          </div>
        </form>
      </SettingsRow>
      {activeCredentials.length > 0 ? (
        <SettingsRow
          title="Rotate Credential"
          description="Rotation replaces the encrypted payload using optimistic versioning. A blank expiry removes the previous expiry."
        >
          <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={(event) => void rotate(event)}>
            <ControlPlaneFormField label="Credential">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                required
                value={rotateCredentialId}
                onChange={(event) => setRotateCredentialId(event.target.value)}
              >
                <option value="">Select a Credential</option>
                {activeCredentials.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name} · v{item.version}
                  </option>
                ))}
              </select>
            </ControlPlaneFormField>
            <ControlPlaneFormField label="New expiry (optional)">
              <Input
                type="datetime-local"
                value={rotateExpiresAt}
                onChange={(event) => setRotateExpiresAt(event.target.value)}
              />
            </ControlPlaneFormField>
            <div className="sm:col-span-2">
              <ControlPlaneFormField label="Replacement secret JSON payload">
                <Textarea
                  autoComplete="off"
                  placeholder={'{"apiKey":"…"}'}
                  required
                  spellCheck={false}
                  value={rotatePayload}
                  onChange={(event) => setRotatePayload(event.target.value)}
                />
              </ControlPlaneFormField>
            </div>
            <div className={cn("sm:col-span-2", !selectedRotation && "text-muted-foreground")}>
              <Button disabled={rotatePending || !selectedRotation} size="sm" type="submit">
                {rotatePending ? "Rotating…" : "Rotate Credential"}
              </Button>
              <ControlPlaneInlineError error={rotateError ?? revokeError} />
            </div>
          </form>
        </SettingsRow>
      ) : revokeError ? (
        <SettingsListRow title="Credential revocation failed" description={String(revokeError)} />
      ) : null}
    </SettingsSection>
  );
}
