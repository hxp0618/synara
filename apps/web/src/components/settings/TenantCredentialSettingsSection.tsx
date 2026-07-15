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
  type ControlPlaneCredential,
  type ControlPlaneCredentialPurpose,
  type ControlPlaneOrganization,
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
  item: ControlPlaneCredential,
  organizationNames: ReadonlyMap<string, string>,
): string {
  const scope = item.organizationId
    ? (organizationNames.get(item.organizationId) ??
      `Organization ${item.organizationId.slice(0, 8)}`)
    : "All organizations";
  const expiry = item.expiresAt
    ? `expires ${new Date(item.expiresAt).toLocaleString()}`
    : "no expiry";
  if (item.purpose === "git") return `Git HTTPS token · ${scope} · ${expiry}`;
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
  const [purpose, setPurpose] = useState<ControlPlaneCredentialPurpose>("provider");
  const [provider, setProvider] = useState("");
  const [credentialType, setCredentialType] = useState("api_key");
  const [organizationId, setOrganizationId] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [createPayload, setCreatePayload] = useState("");
  const [gitHost, setGitHost] = useState("");
  const [gitUsername, setGitUsername] = useState("");
  const [gitToken, setGitToken] = useState("");
  const [createPending, setCreatePending] = useState(false);
  const [createError, setCreateError] = useState<unknown>(null);
  const [rotateCredentialId, setRotateCredentialId] = useState("");
  const [rotateExpiresAt, setRotateExpiresAt] = useState("");
  const [rotatePayload, setRotatePayload] = useState("");
  const [rotateGitHost, setRotateGitHost] = useState("");
  const [rotateGitUsername, setRotateGitUsername] = useState("");
  const [rotateGitToken, setRotateGitToken] = useState("");
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
      const payload =
        purpose === "git"
          ? { host: gitHost, username: gitUsername, token: gitToken }
          : parsePayload(createPayload);
      const parsedExpiresAt = parseExpiry(expiresAt);
      const item = await controlPlaneClient.createCredential(props.tenantId, {
        ...(organizationId ? { organizationId } : {}),
        name,
        purpose,
        provider: purpose === "git" ? "git" : provider,
        credentialType: purpose === "git" ? "https_token" : credentialType,
        payload,
        ...(parsedExpiresAt === null ? {} : { expiresAt: parsedExpiresAt }),
      });
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({ items: [...(current?.items ?? []), item] }),
      );
      setName("");
      setProvider("");
      setExpiresAt("");
      setCreatePayload("");
      setGitHost("");
      setGitUsername("");
    } catch (error) {
      setCreateError(error);
    } finally {
      setGitToken("");
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
      const payload =
        selectedRotation.purpose === "git"
          ? { host: rotateGitHost, username: rotateGitUsername, token: rotateGitToken }
          : parsePayload(rotatePayload);
      const item = await controlPlaneClient.rotateCredential(props.tenantId, selectedRotation.id, {
        expectedVersion: selectedRotation.version,
        payload,
        expiresAt: parseExpiry(rotateExpiresAt),
      });
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({
          items: (current?.items ?? []).map((existing) =>
            existing.id === item.id ? item : existing,
          ),
        }),
      );
      setRotatePayload("");
      setRotateExpiresAt("");
      setRotateGitHost("");
      setRotateGitUsername("");
    } catch (error) {
      setRotateError(error);
    } finally {
      setRotateGitToken("");
      setRotatePending(false);
    }
  };

  const revoke = async (item: ControlPlaneCredential) => {
    if (
      !window.confirm(`Revoke ${item.name}? Running agents will no longer be able to resolve it.`)
    ) {
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
        setRotateGitHost("");
        setRotateGitUsername("");
        setRotateGitToken("");
      }
    } catch (error) {
      setRevokeError(error);
    } finally {
      setRevokePendingId(null);
    }
  };

  return (
    <SettingsSection title="Credentials">
      {credentials.isPending ? <SettingsListRow title="Loading Credentials…" /> : null}
      {credentials.error ? (
        <SettingsListRow
          title="Could not load Credentials"
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
              <span className="text-[10px] tabular-nums text-muted-foreground">
                v{item.version}
              </span>
              <ControlPlaneStatusPill value={item.purpose} />
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
          title="No Credentials"
          description="Add an encrypted Provider or Git Credential with tenant-wide or Organization scope."
        />
      ) : null}
      <SettingsRow
        title="Add encrypted Credential"
        description="Provider and Git Credentials share the encrypted Vault but remain purpose-isolated at Project, Session, and Worker boundaries."
      >
        <form
          className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}
          onSubmit={(event) => void create(event)}
        >
          <ControlPlaneFormField label="Name">
            <Input required value={name} onChange={(event) => setName(event.target.value)} />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Purpose">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={purpose}
              onChange={(event) => {
                const nextPurpose = event.target.value as ControlPlaneCredentialPurpose;
                setPurpose(nextPurpose);
                setCreatePayload("");
                setGitToken("");
                if (nextPurpose === "provider") {
                  setProvider("");
                  setCredentialType("api_key");
                }
              }}
            >
              <option value="provider">Provider runtime</option>
              <option value="git">Private HTTPS Git</option>
            </select>
          </ControlPlaneFormField>
          {purpose === "provider" ? (
            <>
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
            </>
          ) : (
            <>
              <ControlPlaneFormField label="Git host">
                <Input
                  autoCapitalize="none"
                  placeholder="github.com"
                  required
                  value={gitHost}
                  onChange={(event) => setGitHost(event.target.value.toLowerCase())}
                />
              </ControlPlaneFormField>
              <ControlPlaneFormField label="Git username">
                <Input
                  autoCapitalize="none"
                  placeholder="x-access-token"
                  required
                  value={gitUsername}
                  onChange={(event) => setGitUsername(event.target.value)}
                />
              </ControlPlaneFormField>
            </>
          )}
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
            <Input
              type="datetime-local"
              value={expiresAt}
              onChange={(event) => setExpiresAt(event.target.value)}
            />
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            {purpose === "provider" ? (
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
            ) : (
              <ControlPlaneFormField label="Git access token">
                <Input
                  autoComplete="new-password"
                  required
                  type="password"
                  value={gitToken}
                  onChange={(event) => setGitToken(event.target.value)}
                />
              </ControlPlaneFormField>
            )}
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
          <form
            className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}
            onSubmit={(event) => void rotate(event)}
          >
            <ControlPlaneFormField label="Credential">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                required
                value={rotateCredentialId}
                onChange={(event) => {
                  setRotateCredentialId(event.target.value);
                  setRotatePayload("");
                  setRotateGitHost("");
                  setRotateGitUsername("");
                  setRotateGitToken("");
                }}
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
            {selectedRotation?.purpose === "git" ? (
              <>
                <ControlPlaneFormField label="Git host (unchanged)">
                  <Input
                    autoCapitalize="none"
                    placeholder="github.com"
                    required
                    value={rotateGitHost}
                    onChange={(event) => setRotateGitHost(event.target.value.toLowerCase())}
                  />
                </ControlPlaneFormField>
                <ControlPlaneFormField label="Git username">
                  <Input
                    autoCapitalize="none"
                    required
                    value={rotateGitUsername}
                    onChange={(event) => setRotateGitUsername(event.target.value)}
                  />
                </ControlPlaneFormField>
                <div className="sm:col-span-2">
                  <ControlPlaneFormField label="Replacement Git access token">
                    <Input
                      autoComplete="new-password"
                      required
                      type="password"
                      value={rotateGitToken}
                      onChange={(event) => setRotateGitToken(event.target.value)}
                    />
                  </ControlPlaneFormField>
                </div>
              </>
            ) : (
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
            )}
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
