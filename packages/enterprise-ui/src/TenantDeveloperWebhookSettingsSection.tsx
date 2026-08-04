import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  controlPlaneClient,
  type ControlPlaneDeveloperWebhook,
  type ControlPlaneDeveloperWebhookEventType,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

const EVENT_TYPES: ReadonlyArray<ControlPlaneDeveloperWebhookEventType> = [
  "turn.completed",
  "request.opened",
  "approval.requested",
  "execution.completed",
  "execution.failed",
  "execution.cancelled",
  "execution.interrupted",
  "execution.suspended",
];

export function developerWebhookQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "developer-webhooks"] as const;
}

export function developerWebhookDeliveryQueryKey(tenantId: string, webhookId: string) {
  return [
    "control-plane",
    "tenants",
    tenantId,
    "developer-webhooks",
    webhookId,
    "deliveries",
  ] as const;
}

export function TenantDeveloperWebhookSettingsSection(props: {
  tenantId: string;
  canManage: boolean;
}) {
  const { Button, InlineError, SettingsListRow, SettingsRow, SettingsSection } =
    useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const endpoints = useQuery({
    queryKey: developerWebhookQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.listDeveloperWebhooks(props.tenantId),
    retry: false,
  });
  const [issuedSecret, setIssuedSecret] = useState("");
  return (
    <SettingsSection title="Developer Webhooks">
      <SettingsRow
        title="Signed Event delivery"
        description="Deliver a thin, versioned Event envelope to a server-side HTTPS endpoint. Secrets are shown once; each delivery uses an HMAC signature, a stable idempotency key, bounded retries, and a dead-letter queue."
      >
        {props.canManage ? (
          <WebhookForm
            tenantId={props.tenantId}
            onCreated={(endpoint, secret) => {
              queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneDeveloperWebhook> }>(
                developerWebhookQueryKey(props.tenantId),
                (current) => ({ items: [endpoint, ...(current?.items ?? [])] }),
              );
              setIssuedSecret(secret);
            }}
          />
        ) : null}
      </SettingsRow>
      {issuedSecret ? (
        <SettingsRow
          title="Copy the signing secret now"
          description="Polaris cannot display this secret again. Store it in your secret manager before clearing it."
          control={
            <Button size="sm" variant="outline" onClick={() => setIssuedSecret("")}>
              Clear secret
            </Button>
          }
        >
          <code className="block break-all rounded-md border border-border bg-muted/40 p-3 text-xs text-foreground">
            {issuedSecret}
          </code>
        </SettingsRow>
      ) : null}
      {endpoints.isPending ? <SettingsListRow title="Loading Webhooks…" /> : null}
      {endpoints.data?.items.map((endpoint) => (
        <WebhookRow
          key={endpoint.id}
          canManage={props.canManage}
          endpoint={endpoint}
          tenantId={props.tenantId}
          onSecret={setIssuedSecret}
        />
      ))}
      {endpoints.data?.items.length === 0 ? (
        <SettingsListRow
          title="No Webhook endpoints"
          description="Add an HTTPS receiver when your integration is ready to verify signatures and deduplicate delivery IDs."
        />
      ) : null}
      <InlineError error={endpoints.error} />
    </SettingsSection>
  );
}

function WebhookForm(props: {
  tenantId: string;
  onCreated: (endpoint: ControlPlaneDeveloperWebhook, secret: string) => void;
}) {
  const { Button, FormField, InlineError, Input, formGridClassName } = useEnterpriseUiHost();
  const [name, setName] = useState("");
  const [url, setURL] = useState("");
  const [eventTypes, setEventTypes] = useState<
    ReadonlyArray<ControlPlaneDeveloperWebhookEventType>
  >(["turn.completed", "execution.completed", "execution.failed"]);
  const create = useMutation({
    mutationFn: () =>
      controlPlaneClient.createDeveloperWebhook(props.tenantId, { name, url, eventTypes }),
    onSuccess: (issued) => {
      props.onCreated(issued.endpoint, issued.secret);
      setName("");
      setURL("");
    },
  });
  return (
    <form
      className={formGridClassName}
      data-testid="developer-webhook-form"
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <FormField label="Name">
        <Input required value={name} onChange={(event) => setName(event.target.value)} />
      </FormField>
      <FormField label="HTTPS endpoint">
        <Input
          required
          type="url"
          placeholder="https://example.com/webhooks/polaris"
          value={url}
          onChange={(event) => setURL(event.target.value)}
        />
      </FormField>
      <div className="space-y-2 sm:col-span-2">
        {EVENT_TYPES.map((type) => (
          <label key={type} className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              checked={eventTypes.includes(type)}
              type="checkbox"
              onChange={(event) =>
                setEventTypes((current) =>
                  event.target.checked
                    ? [...current, type]
                    : current.filter((item) => item !== type),
                )
              }
            />
            <span className="font-medium text-foreground">{type}</span>
          </label>
        ))}
      </div>
      <div className="sm:col-span-2">
        <Button disabled={create.isPending || eventTypes.length === 0} size="sm" type="submit">
          {create.isPending ? "Creating Webhook…" : "Create Webhook"}
        </Button>
        <InlineError error={create.error} />
      </div>
    </form>
  );
}

function WebhookRow(props: {
  tenantId: string;
  endpoint: ControlPlaneDeveloperWebhook;
  canManage: boolean;
  onSecret: (secret: string) => void;
}) {
  const { Button, SettingsListRow, StatusPill } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const deliveryKey = developerWebhookDeliveryQueryKey(props.tenantId, props.endpoint.id);
  const deliveries = useQuery({
    queryKey: deliveryKey,
    queryFn: () =>
      controlPlaneClient.listDeveloperWebhookDeliveries(props.tenantId, props.endpoint.id, 10),
    retry: false,
  });
  const updateCached = (next: ControlPlaneDeveloperWebhook) =>
    queryClient.setQueryData<{ items: ReadonlyArray<ControlPlaneDeveloperWebhook> }>(
      developerWebhookQueryKey(props.tenantId),
      (current) => ({
        items: (current?.items ?? []).map((item) => (item.id === next.id ? next : item)),
      }),
    );
  const toggle = useMutation({
    mutationFn: () =>
      controlPlaneClient.setDeveloperWebhookEnabled(
        props.tenantId,
        props.endpoint.id,
        props.endpoint.status !== "active",
      ),
    onSuccess: updateCached,
  });
  const rotate = useMutation({
    mutationFn: () =>
      controlPlaneClient.rotateDeveloperWebhookSecret(props.tenantId, props.endpoint.id),
    onSuccess: (issued) => {
      updateCached(issued.endpoint);
      props.onSecret(issued.secret);
    },
  });
  const revoke = useMutation({
    mutationFn: () => controlPlaneClient.revokeDeveloperWebhook(props.tenantId, props.endpoint.id),
    onSuccess: updateCached,
  });
  const replay = useMutation({
    mutationFn: (deliveryId: string) =>
      controlPlaneClient.replayDeveloperWebhookDelivery(
        props.tenantId,
        props.endpoint.id,
        deliveryId,
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: deliveryKey }),
  });
  return (
    <>
      <SettingsListRow
        title={props.endpoint.name}
        description={`${props.endpoint.url} · ${props.endpoint.eventTypes.join(", ")} · secret v${props.endpoint.secretVersion} · last delivered ${props.endpoint.lastDeliveredAt ? new Date(props.endpoint.lastDeliveredAt).toLocaleString() : "never"}${props.endpoint.lastFailureSummary ? ` · last error ${props.endpoint.lastFailureSummary}` : ""}`}
        actions={
          <span className="flex items-center gap-2">
            <StatusPill value={props.endpoint.status} />
            {props.canManage && props.endpoint.status !== "revoked" ? (
              <>
                <Button
                  disabled={toggle.isPending}
                  size="sm"
                  variant="outline"
                  onClick={() => toggle.mutate()}
                >
                  {props.endpoint.status === "active" ? "Disable" : "Enable"}
                </Button>
                <Button
                  disabled={rotate.isPending}
                  size="sm"
                  variant="outline"
                  onClick={() => rotate.mutate()}
                >
                  Rotate secret
                </Button>
                <Button
                  disabled={revoke.isPending}
                  size="sm"
                  variant="outline"
                  onClick={() => revoke.mutate()}
                >
                  Revoke
                </Button>
              </>
            ) : null}
          </span>
        }
      />
      {deliveries.data?.items.map((delivery) => (
        <SettingsListRow
          key={delivery.id}
          title={`Delivery ${delivery.id.slice(0, 8)} · ${delivery.eventType}`}
          description={`sequence ${delivery.sequence} · ${delivery.attempts} attempts · ${new Date(delivery.createdAt).toLocaleString()}${delivery.lastError ? ` · ${delivery.lastError}` : ""}`}
          actions={
            <span className="flex items-center gap-2">
              <StatusPill value={delivery.status} />
              {props.canManage && delivery.status === "dead-letter" ? (
                <Button
                  disabled={replay.isPending}
                  size="sm"
                  variant="outline"
                  onClick={() => replay.mutate(delivery.id)}
                >
                  Replay
                </Button>
              ) : null}
            </span>
          }
        />
      ))}
    </>
  );
}
