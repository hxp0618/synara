import { ControlPlaneFormField } from "~/components/settings/ControlPlaneSettingsPrimitives";
import { Input } from "~/components/ui/input";
import { Textarea } from "~/components/ui/textarea";
import type { CredentialFormKind, CredentialPayloadDraft } from "~/lib/credentialPayloadForm";

export function CredentialPayloadFields(props: {
  draft: CredentialPayloadDraft;
  kind: CredentialFormKind;
  mode: "create" | "rotate";
  onChange: (draft: CredentialPayloadDraft) => void;
}) {
  const update = <Key extends keyof CredentialPayloadDraft>(
    key: Key,
    value: CredentialPayloadDraft[Key],
  ) => props.onChange({ ...props.draft, [key]: value });
  const replacement = props.mode === "rotate" ? "Replacement " : "";

  switch (props.kind) {
    case "provider":
      return (
        <>
          <ControlPlaneFormField label="Provider">
            <Input
              autoCapitalize="none"
              placeholder="openai"
              readOnly={props.mode === "rotate"}
              required
              value={props.draft.provider}
              onChange={(event) => update("provider", event.target.value.toLowerCase())}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Credential type">
            <Input
              autoCapitalize="none"
              placeholder="api_key"
              readOnly={props.mode === "rotate"}
              required
              value={props.draft.credentialType}
              onChange={(event) => update("credentialType", event.target.value.toLowerCase())}
            />
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            <ControlPlaneFormField label={`${replacement}secret JSON payload`}>
              <Textarea
                autoComplete="off"
                placeholder={'{"apiKey":"…"}'}
                required
                spellCheck={false}
                value={props.draft.jsonPayload}
                onChange={(event) => update("jsonPayload", event.target.value)}
              />
            </ControlPlaneFormField>
          </div>
        </>
      );
    case "git_https":
      return (
        <>
          <HostField value={props.draft.host} onChange={(value) => update("host", value)} />
          <ControlPlaneFormField label="Git username">
            <Input
              autoCapitalize="none"
              placeholder="x-access-token"
              required
              value={props.draft.username}
              onChange={(event) => update("username", event.target.value)}
            />
          </ControlPlaneFormField>
          <SecretField
            label={`${replacement}Git access token`}
            value={props.draft.secret}
            onChange={(value) => update("secret", value)}
          />
        </>
      );
    case "git_ssh":
      return (
        <>
          <HostField value={props.draft.host} onChange={(value) => update("host", value)} />
          <ControlPlaneFormField label="SSH port">
            <Input
              inputMode="numeric"
              max={65_535}
              min={1}
              required
              type="number"
              value={props.draft.port}
              onChange={(event) => update("port", event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="SSH username">
            <Input
              autoCapitalize="none"
              placeholder="git"
              required
              value={props.draft.username}
              onChange={(event) => update("username", event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Private key passphrase (optional)">
            <Input
              autoComplete="new-password"
              type="password"
              value={props.draft.privateKeyPassphrase}
              onChange={(event) => update("privateKeyPassphrase", event.target.value)}
            />
          </ControlPlaneFormField>
          <div className="sm:col-span-2">
            <ControlPlaneFormField label={`${replacement}SSH private key`}>
              <Textarea
                autoComplete="off"
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                required
                spellCheck={false}
                value={props.draft.privateKey}
                onChange={(event) => update("privateKey", event.target.value)}
              />
            </ControlPlaneFormField>
          </div>
          <div className="sm:col-span-2">
            <ControlPlaneFormField label="Pinned SSH host public key">
              <Textarea
                autoComplete="off"
                placeholder="ssh-ed25519 AAAAC3…"
                required
                spellCheck={false}
                value={props.draft.hostKey}
                onChange={(event) => update("hostKey", event.target.value)}
              />
            </ControlPlaneFormField>
          </div>
          <p className="text-xs text-muted-foreground sm:col-span-2">
            Paste the exact host public key without a comment. Synara pins it and refuses unknown or
            changed hosts.
          </p>
        </>
      );
    case "registry_basic":
      return (
        <>
          <RegistryHostField value={props.draft.host} onChange={(value) => update("host", value)} />
          <ControlPlaneFormField label="Registry username">
            <Input
              autoCapitalize="none"
              required
              value={props.draft.username}
              onChange={(event) => update("username", event.target.value)}
            />
          </ControlPlaneFormField>
          <SecretField
            label={`${replacement}Registry password`}
            value={props.draft.secret}
            onChange={(value) => update("secret", value)}
          />
        </>
      );
    case "registry_bearer":
      return (
        <>
          <RegistryHostField value={props.draft.host} onChange={(value) => update("host", value)} />
          <SecretField
            label={`${replacement}Registry bearer token`}
            value={props.draft.secret}
            onChange={(value) => update("secret", value)}
          />
        </>
      );
    case "package_npm":
      return (
        <>
          <ControlPlaneFormField label="npm registry URL">
            <Input
              autoCapitalize="none"
              inputMode="url"
              placeholder="https://registry.npmjs.org/"
              required
              type="url"
              value={props.draft.endpointUrl}
              onChange={(event) => update("endpointUrl", event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Allowed scopes (optional)">
            <Input
              autoCapitalize="none"
              placeholder="@company, @internal"
              value={props.draft.scopes}
              onChange={(event) => update("scopes", event.target.value.toLowerCase())}
            />
          </ControlPlaneFormField>
          <SecretField
            label={`${replacement}npm token`}
            value={props.draft.secret}
            onChange={(value) => update("secret", value)}
          />
        </>
      );
    case "package_pypi":
      return (
        <>
          <ControlPlaneFormField label="PyPI index URL">
            <Input
              autoCapitalize="none"
              inputMode="url"
              placeholder="https://pypi.org/simple/"
              required
              type="url"
              value={props.draft.endpointUrl}
              onChange={(event) => update("endpointUrl", event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="PyPI username">
            <Input
              autoCapitalize="none"
              placeholder="__token__"
              required
              value={props.draft.username}
              onChange={(event) => update("username", event.target.value)}
            />
          </ControlPlaneFormField>
          <SecretField
            label={`${replacement}PyPI token`}
            value={props.draft.secret}
            onChange={(value) => update("secret", value)}
          />
        </>
      );
  }
}

function HostField(props: { value: string; onChange: (value: string) => void }) {
  return (
    <ControlPlaneFormField label="Git host">
      <Input
        autoCapitalize="none"
        placeholder="github.com"
        required
        value={props.value}
        onChange={(event) => props.onChange(event.target.value.toLowerCase())}
      />
    </ControlPlaneFormField>
  );
}

function RegistryHostField(props: { value: string; onChange: (value: string) => void }) {
  return (
    <ControlPlaneFormField label="Registry host">
      <Input
        autoCapitalize="none"
        placeholder="registry.example.com"
        required
        value={props.value}
        onChange={(event) => props.onChange(event.target.value.toLowerCase())}
      />
    </ControlPlaneFormField>
  );
}

function SecretField(props: { label: string; value: string; onChange: (value: string) => void }) {
  return (
    <div className="sm:col-span-2">
      <ControlPlaneFormField label={props.label}>
        <Input
          autoComplete="new-password"
          required
          type="password"
          value={props.value}
          onChange={(event) => props.onChange(event.target.value)}
        />
      </ControlPlaneFormField>
    </div>
  );
}
