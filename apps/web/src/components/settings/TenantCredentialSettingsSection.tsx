import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneInlineError,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { CredentialPayloadFields } from "~/components/settings/CredentialPayloadFields";
import {
  SettingsListRow,
  SettingsRow,
  SettingsSection,
} from "~/components/settings/SettingsPanelPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import {
  controlPlaneClient,
  type ControlPlaneCredential,
  type ControlPlaneCredentialScope,
  type ControlPlaneOrganization,
  type ControlPlaneTenantMember,
} from "~/lib/controlPlaneClient";
import {
  buildCredentialPayload,
  CREDENTIAL_FORM_KIND_OPTIONS,
  clearCredentialSecrets,
  createCredentialPayloadDraft,
  credentialFormKindForCredential,
  describeCredentialForm,
  type CredentialFormKind,
} from "~/lib/credentialPayloadForm";
import { cn } from "~/lib/utils";

export function credentialsQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "credentials"] as const;
}

export function credentialScopePolicyQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "provider-credential-scope-policy"] as const;
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
  memberNames: ReadonlyMap<string, string>,
): string {
  let scope: string;
  switch (item.scope) {
    case "user":
      scope = item.scopeUserId
        ? `User ${memberNames.get(item.scopeUserId) ?? item.scopeUserId.slice(0, 8)}`
        : "Invalid user scope";
      break;
    case "organization":
      scope = item.organizationId
        ? (organizationNames.get(item.organizationId) ??
          `Organization ${item.organizationId.slice(0, 8)}`)
        : "Invalid Organization scope";
      break;
    case "tenant": {
      const selectors = [
        item.selectorOrganizationId
          ? (organizationNames.get(item.selectorOrganizationId) ??
            `Organization ${item.selectorOrganizationId.slice(0, 8)}`)
          : null,
        item.selectorModel ? `model ${item.selectorModel}` : null,
      ].filter((value): value is string => value !== null);
      scope = selectors.length > 0 ? `Tenant · ${selectors.join(" · ")}` : "Tenant";
      break;
    }
    case "platform":
      scope = "Platform fallback";
      break;
  }
  const expiry = item.expiresAt
    ? `expires ${new Date(item.expiresAt).toLocaleString()}`
    : "no expiry";
  if (item.purpose === "git") {
    return `Git ${item.credentialType === "ssh_key" ? "SSH key" : "HTTPS token"} · ${scope} · ${expiry}`;
  }
  if (item.purpose === "registry") {
    return `OCI Registry · ${item.credentialType.replaceAll("_", " ")} · ${scope} · ${expiry}`;
  }
  if (item.purpose === "package") {
    return `${item.provider} packages · ${item.credentialType.replaceAll("_", " ")} · ${scope} · ${expiry}`;
  }
  const automatic = item.autoSelectEnabled ? "automatic" : "explicit only";
  return `${item.provider} · ${item.credentialType.replaceAll("_", " ")} · ${scope} · ${automatic} · ${expiry}`;
}

export function TenantCredentialSettingsSection(props: {
  tenantId: string;
  organizations: ReadonlyArray<ControlPlaneOrganization>;
  members: ReadonlyArray<ControlPlaneTenantMember>;
}) {
  const queryClient = useQueryClient();
  const credentials = useQuery({
    queryKey: credentialsQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listCredentials(props.tenantId),
    retry: false,
  });
  const scopePolicy = useQuery({
    queryKey: credentialScopePolicyQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getProviderCredentialScopePolicy(props.tenantId),
    retry: false,
  });
  const organizationNames = useMemo(
    () => new Map(props.organizations.map((item) => [item.id, item.name])),
    [props.organizations],
  );
  const memberNames = useMemo(
    () => new Map(props.members.map((item) => [item.userId, item.displayName || item.email])),
    [props.members],
  );
  const [name, setName] = useState("");
  const [createKind, setCreateKind] = useState<CredentialFormKind>("provider");
  const [createDraft, setCreateDraft] = useState(() => createCredentialPayloadDraft("provider"));
  const [scope, setScope] = useState<ControlPlaneCredentialScope>("tenant");
  const [scopeUserId, setScopeUserId] = useState("");
  const [organizationId, setOrganizationId] = useState("");
  const [selectorOrganizationId, setSelectorOrganizationId] = useState("");
  const [selectorModel, setSelectorModel] = useState("");
  const [autoSelectEnabled, setAutoSelectEnabled] = useState(false);
  const [expiresAt, setExpiresAt] = useState("");
  const [createPending, setCreatePending] = useState(false);
  const [createError, setCreateError] = useState<unknown>(null);
  const [rotateCredentialId, setRotateCredentialId] = useState("");
  const [rotateExpiresAt, setRotateExpiresAt] = useState("");
  const [rotateDraft, setRotateDraft] = useState(() => createCredentialPayloadDraft("provider"));
  const [rotatePending, setRotatePending] = useState(false);
  const [rotateError, setRotateError] = useState<unknown>(null);
  const [revokePendingId, setRevokePendingId] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<unknown>(null);
  const [autoSelectPendingId, setAutoSelectPendingId] = useState<string | null>(null);
  const [autoSelectError, setAutoSelectError] = useState<unknown>(null);
  const [policyPending, setPolicyPending] = useState(false);
  const [policyError, setPolicyError] = useState<unknown>(null);

  const activeCredentials = credentials.data?.items.filter((item) => item.revokedAt === null) ?? [];
  const selectedRotation = activeCredentials.find((item) => item.id === rotateCredentialId) ?? null;
  const selectedRotationKind = selectedRotation
    ? credentialFormKindForCredential(selectedRotation)
    : null;
  const createDescriptor = describeCredentialForm(createKind, createDraft);

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setCreateError(null);
    setCreatePending(true);
    const submittedKind = createKind;
    try {
      const descriptor = describeCredentialForm(submittedKind, createDraft);
      const payload = buildCredentialPayload(submittedKind, createDraft);
      const parsedExpiresAt = parseExpiry(expiresAt);
      const item = await controlPlaneClient.createCredential(props.tenantId, {
        scope,
        ...(scope === "user" && scopeUserId ? { scopeUserId } : {}),
        ...(scope === "organization" && organizationId ? { organizationId } : {}),
        ...(scope === "tenant" && selectorOrganizationId ? { selectorOrganizationId } : {}),
        ...(scope === "tenant" && selectorModel.trim()
          ? { selectorModel: selectorModel.trim() }
          : {}),
        ...(descriptor.purpose === "provider" ? { autoSelectEnabled } : {}),
        name,
        purpose: descriptor.purpose,
        provider: descriptor.provider,
        credentialType: descriptor.credentialType,
        payload,
        ...(parsedExpiresAt === null ? {} : { expiresAt: parsedExpiresAt }),
      });
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({ items: [...(current?.items ?? []), item] }),
      );
      setName("");
      setScope("tenant");
      setScopeUserId("");
      setOrganizationId("");
      setSelectorOrganizationId("");
      setSelectorModel("");
      setAutoSelectEnabled(false);
      setExpiresAt("");
      setCreateDraft(createCredentialPayloadDraft(submittedKind));
    } catch (error) {
      setCreateError(error);
    } finally {
      if (submittedKind !== "provider") {
        setCreateDraft((current) => clearCredentialSecrets(current));
      }
      setCreatePending(false);
    }
  };

  const rotate = async (event: FormEvent) => {
    event.preventDefault();
    setRotateError(null);
    if (!selectedRotation || !selectedRotationKind) {
      setRotateError(new Error("Select an active Credential to rotate."));
      return;
    }
    setRotatePending(true);
    try {
      const payload = buildCredentialPayload(selectedRotationKind, rotateDraft);
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
      setRotateExpiresAt("");
      const nextDraft = createCredentialPayloadDraft(selectedRotationKind);
      setRotateDraft(
        selectedRotationKind === "provider"
          ? {
              ...nextDraft,
              provider: selectedRotation.provider,
              credentialType: selectedRotation.credentialType,
            }
          : nextDraft,
      );
    } catch (error) {
      setRotateError(error);
    } finally {
      if (selectedRotationKind !== "provider") {
        setRotateDraft((current) => clearCredentialSecrets(current));
      }
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
        setRotateDraft(createCredentialPayloadDraft("provider"));
      }
    } catch (error) {
      setRevokeError(error);
    } finally {
      setRevokePendingId(null);
    }
  };

  const setAutoSelect = async (item: ControlPlaneCredential) => {
    setAutoSelectError(null);
    setAutoSelectPendingId(item.id);
    try {
      const updated = await controlPlaneClient.setCredentialAutoSelect(
        props.tenantId,
        item.id,
        !item.autoSelectEnabled,
      );
      queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneCredential> }>(
        credentialsQueryKey(props.tenantId),
        (current) => ({
          items: (current?.items ?? []).map((existing) =>
            existing.id === updated.id ? updated : existing,
          ),
        }),
      );
    } catch (error) {
      setAutoSelectError(error);
    } finally {
      setAutoSelectPendingId(null);
    }
  };

  const updateScopePolicy = async (
    platformCredentialsEnabled: boolean,
    platformCredentialAutoSelect: boolean,
  ) => {
    setPolicyError(null);
    setPolicyPending(true);
    try {
      const updated = await controlPlaneClient.updateProviderCredentialScopePolicy(props.tenantId, {
        platformCredentialsEnabled,
        platformCredentialAutoSelect,
      });
      queryClient.setQueryData(credentialScopePolicyQueryKey(props.tenantId), updated);
    } catch (error) {
      setPolicyError(error);
    } finally {
      setPolicyPending(false);
    }
  };

  return (
    <SettingsSection title="Credentials">
      <SettingsRow
        title="Platform Credential policy"
        description="Enterprise-only fallback. Tenant policy and each Credential must both opt in before automatic selection is allowed."
      >
        {scopePolicy.isPending ? (
          <span className="text-xs text-muted-foreground">Loading policy…</span>
        ) : scopePolicy.error ? (
          <div>
            <Button size="sm" variant="outline" onClick={() => void scopePolicy.refetch()}>
              Retry
            </Button>
            <ControlPlaneInlineError error={scopePolicy.error} />
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <Button
              disabled={policyPending}
              size="sm"
              variant="outline"
              onClick={() =>
                void updateScopePolicy(!scopePolicy.data!.platformCredentialsEnabled, false)
              }
            >
              {scopePolicy.data!.platformCredentialsEnabled
                ? "Disable Platform Credentials"
                : "Enable Platform Credentials"}
            </Button>
            <Button
              disabled={policyPending || !scopePolicy.data!.platformCredentialsEnabled}
              size="sm"
              variant="outline"
              onClick={() =>
                void updateScopePolicy(true, !scopePolicy.data!.platformCredentialAutoSelect)
              }
            >
              {scopePolicy.data!.platformCredentialAutoSelect
                ? "Disable automatic fallback"
                : "Enable automatic fallback"}
            </Button>
            <ControlPlaneInlineError error={policyError} />
          </div>
        )}
      </SettingsRow>
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
          description={credentialDescription(item, organizationNames, memberNames)}
          actions={
            <span className="flex flex-wrap items-center justify-end gap-1.5">
              <span className="text-[10px] tabular-nums text-muted-foreground">
                v{item.version}
              </span>
              <ControlPlaneStatusPill value={item.purpose} />
              <ControlPlaneStatusPill value={item.scope} />
              <ControlPlaneStatusPill value={item.revokedAt ? "revoked" : "active"} />
              {item.revokedAt === null ? (
                <>
                  {item.purpose === "provider" ? (
                    <Button
                      disabled={autoSelectPendingId === item.id}
                      size="sm"
                      variant="outline"
                      onClick={() => void setAutoSelect(item)}
                    >
                      {autoSelectPendingId === item.id
                        ? "Saving…"
                        : item.autoSelectEnabled
                          ? "Automatic on"
                          : "Explicit only"}
                    </Button>
                  ) : null}
                  <Button
                    className="text-destructive hover:text-destructive"
                    disabled={revokePendingId === item.id}
                    size="sm"
                    variant="outline"
                    onClick={() => void revoke(item)}
                  >
                    {revokePendingId === item.id ? "Revoking…" : "Revoke"}
                  </Button>
                </>
              ) : null}
            </span>
          }
        />
      ))}
      {credentials.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Credentials"
          description="Add an encrypted Provider, Git, Registry, or Package Credential with the narrowest usable scope."
        />
      ) : null}
      {autoSelectError ? (
        <SettingsListRow
          title="Credential automatic selection update failed"
          description={String(autoSelectError)}
        />
      ) : null}
      <SettingsRow
        title="Add encrypted Credential"
        description="Provider, Git, Registry, and Package Credentials share the encrypted Vault but remain purpose-isolated at Session, Project, stage, and Worker boundaries."
      >
        <form
          className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}
          onSubmit={(event) => void create(event)}
        >
          <ControlPlaneFormField label="Name">
            <Input required value={name} onChange={(event) => setName(event.target.value)} />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Credential kind">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              disabled={createPending}
              value={createKind}
              onChange={(event) => {
                const nextKind = event.target.value as CredentialFormKind;
                setCreateKind(nextKind);
                setCreateDraft(createCredentialPayloadDraft(nextKind));
                setScope("tenant");
                setScopeUserId("");
                setOrganizationId("");
                setSelectorOrganizationId("");
                setSelectorModel("");
                setAutoSelectEnabled(false);
              }}
            >
              {CREDENTIAL_FORM_KIND_OPTIONS.map((option) => (
                <option key={option.kind} value={option.kind}>
                  {option.label}
                </option>
              ))}
            </select>
          </ControlPlaneFormField>
          <CredentialPayloadFields
            draft={createDraft}
            kind={createKind}
            mode="create"
            onChange={setCreateDraft}
          />
          <ControlPlaneFormField label="Scope">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={scope}
              onChange={(event) => {
                setScope(event.target.value as ControlPlaneCredentialScope);
                setScopeUserId("");
                setOrganizationId("");
                setSelectorOrganizationId("");
                setSelectorModel("");
              }}
            >
              {createDescriptor.purpose === "provider" ? <option value="user">User</option> : null}
              <option value="organization">Organization</option>
              <option value="tenant">Tenant</option>
              {createDescriptor.purpose === "provider" ? (
                <option value="platform">Platform fallback</option>
              ) : null}
            </select>
          </ControlPlaneFormField>
          {scope === "user" ? (
            <ControlPlaneFormField label="User">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                required
                value={scopeUserId}
                onChange={(event) => setScopeUserId(event.target.value)}
              >
                <option value="">Select an active Tenant member</option>
                {props.members
                  .filter((member) => member.status === "active")
                  .map((member) => (
                    <option key={member.userId} value={member.userId}>
                      {member.displayName || member.email}
                    </option>
                  ))}
              </select>
            </ControlPlaneFormField>
          ) : null}
          {scope === "organization" ? (
            <ControlPlaneFormField label="Organization">
              <select
                className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                required
                value={organizationId}
                onChange={(event) => setOrganizationId(event.target.value)}
              >
                <option value="">Select an Organization</option>
                {props.organizations.map((organization) => (
                  <option key={organization.id} value={organization.id}>
                    {organization.name}
                  </option>
                ))}
              </select>
            </ControlPlaneFormField>
          ) : null}
          {scope === "tenant" && createDescriptor.purpose === "provider" ? (
            <>
              <ControlPlaneFormField label="Organization selector (optional)">
                <select
                  className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
                  value={selectorOrganizationId}
                  onChange={(event) => setSelectorOrganizationId(event.target.value)}
                >
                  <option value="">Any Organization</option>
                  {props.organizations.map((organization) => (
                    <option key={organization.id} value={organization.id}>
                      {organization.name}
                    </option>
                  ))}
                </select>
              </ControlPlaneFormField>
              <ControlPlaneFormField label="Model selector (optional)">
                <Input
                  autoCapitalize="none"
                  placeholder="gpt-5.6"
                  value={selectorModel}
                  onChange={(event) => setSelectorModel(event.target.value)}
                />
              </ControlPlaneFormField>
            </>
          ) : null}
          {scope === "platform" ? (
            <div className="text-xs text-muted-foreground sm:col-span-2">
              Platform scope requires an enterprise installation, an enterprise Tenant plan, and the
              explicit policy above. It is still Tenant-owned and never shared across Tenant
              boundaries.
            </div>
          ) : null}
          {createDescriptor.purpose === "provider" ? (
            <label className="flex items-center gap-2 text-xs text-muted-foreground sm:col-span-2">
              <input
                checked={autoSelectEnabled}
                type="checkbox"
                onChange={(event) => setAutoSelectEnabled(event.target.checked)}
              />
              <span>
                Allow automatic selection. Explicit Session binding always takes precedence;
                ambiguous Credentials at the same scope fail closed.
              </span>
            </label>
          ) : null}
          <ControlPlaneFormField label="Expiry (optional)">
            <Input
              type="datetime-local"
              value={expiresAt}
              onChange={(event) => setExpiresAt(event.target.value)}
            />
          </ControlPlaneFormField>
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
                  const credentialId = event.target.value;
                  setRotateCredentialId(credentialId);
                  setRotateExpiresAt("");
                  const credential = activeCredentials.find((item) => item.id === credentialId);
                  const kind = credential ? credentialFormKindForCredential(credential) : null;
                  const nextDraft = createCredentialPayloadDraft(kind ?? "provider");
                  setRotateDraft(
                    credential?.purpose === "provider"
                      ? {
                          ...nextDraft,
                          provider: credential.provider,
                          credentialType: credential.credentialType,
                        }
                      : nextDraft,
                  );
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
            {selectedRotationKind ? (
              <CredentialPayloadFields
                draft={rotateDraft}
                kind={selectedRotationKind}
                mode="rotate"
                onChange={setRotateDraft}
              />
            ) : selectedRotation ? (
              <p className="text-xs text-destructive sm:col-span-2">
                This Credential uses an unsupported purpose/type combination and cannot be rotated
                from this client.
              </p>
            ) : null}
            <div className={cn("sm:col-span-2", !selectedRotation && "text-muted-foreground")}>
              <Button
                disabled={rotatePending || !selectedRotation || !selectedRotationKind}
                size="sm"
                type="submit"
              >
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
