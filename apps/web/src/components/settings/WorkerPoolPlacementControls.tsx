import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  CONTROL_PLANE_FORM_GRID_CLASS_NAME,
  CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME,
  ControlPlaneFormField,
  ControlPlaneStatusPill,
} from "~/components/settings/ControlPlaneSettingsPrimitives";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import {
  controlPlaneClient,
  type ControlPlaneWorkerPoolInput,
  type ControlPlaneWorkerPool,
  type ControlPlaneWorkerPoolStatus,
  type ControlPlaneExecutionPlacementState,
  type ControlPlaneExecutionTarget,
} from "~/lib/controlPlaneClient";

export function workerPoolPlacementQueryKey(tenantId: string, targetId: string) {
  return ["control-plane", "execution-placement", tenantId, targetId] as const;
}

export type WorkerPoolPlacementPolicyField =
  | "defaultPoolId"
  | "balancedPoolId"
  | "lowLatencyPoolId";

export function buildPlacementPolicyInput(
  state: ControlPlaneExecutionPlacementState,
  field: WorkerPoolPlacementPolicyField,
  value: string,
) {
  return {
    expectedVersion: state.policy.version,
    defaultPoolId: field === "defaultPoolId" ? value : state.policy.defaultPoolId,
    balancedPoolId: field === "balancedPoolId" ? value || null : state.policy.balancedPoolId,
    lowLatencyPoolId: field === "lowLatencyPoolId" ? value || null : state.policy.lowLatencyPoolId,
  };
}

export function buildWarmPoolInput(input: {
  name: string;
  desiredIdleUnits: string;
  maxActiveUnits: string;
}): ControlPlaneWorkerPoolInput {
  return {
    name: input.name.trim(),
    mode: "warm",
    capacityClass: "interactive",
    tenantIsolation: "pinned",
    clusterId: "",
    region: "",
    namespace: "",
    desiredIdleUnits: Number(input.desiredIdleUnits),
    minIdleUnits: 0,
    maxActiveUnits: Number(input.maxActiveUnits),
    schedulingTemplate: {},
    status: "active",
  };
}

export function buildWorkerPoolUpdateInput(
  pool: ControlPlaneWorkerPool,
  input: {
    desiredIdleUnits: string;
    maxActiveUnits: string;
    status: ControlPlaneWorkerPoolStatus;
  },
) {
  return {
    expectedVersion: pool.version,
    name: pool.name,
    mode: pool.mode,
    capacityClass: pool.capacityClass,
    tenantIsolation: pool.tenantIsolation,
    clusterId: pool.clusterId,
    region: pool.region,
    namespace: pool.namespace,
    desiredIdleUnits: Number(input.desiredIdleUnits),
    minIdleUnits: pool.minIdleUnits,
    maxActiveUnits: Number(input.maxActiveUnits),
    schedulingTemplate: pool.schedulingTemplate,
    status: input.status,
  };
}

export function WorkerPoolPlacementControls(props: {
  tenantId: string;
  target: ControlPlaneExecutionTarget;
  canManage: boolean;
  enabled: boolean;
}) {
  const queryClient = useQueryClient();
  const queryKey = workerPoolPlacementQueryKey(props.tenantId, props.target.id);
  const canMutatePlacement = props.canManage && props.target.tenantId !== null;
  const placement = useQuery({
    queryKey,
    queryFn: () => controlPlaneClient.getExecutionPlacement(props.tenantId, props.target.id),
    enabled: props.enabled,
  });
  const updatePolicy = useMutation({
    mutationFn: (input: {
      expectedVersion: number;
      defaultPoolId: string;
      balancedPoolId: string | null;
      lowLatencyPoolId: string | null;
    }) => controlPlaneClient.updateExecutionPlacementPolicy(props.tenantId, props.target.id, input),
    onSuccess: (next) => queryClient.setQueryData(queryKey, next),
  });

  const state = placement.data;
  const updateMapping = (field: WorkerPoolPlacementPolicyField, value: string) => {
    if (!state || updatePolicy.isPending) return;
    updatePolicy.mutate(buildPlacementPolicyInput(state, field, value));
  };

  return (
    <section className="space-y-2 border-t border-border/70 pt-2">
      <header>
        <p className="font-medium text-foreground">Worker Pools & Placement</p>
        <p className="text-muted-foreground">
          Target-local capacity contracts. Warm capacity is counted only after a warm Worker
          registers; requested Session mode alone is not capacity proof.
        </p>
      </header>
      {placement.isLoading ? (
        <p className="text-muted-foreground">Loading placement state…</p>
      ) : placement.error ? (
        <p className="text-destructive">{errorMessage(placement.error)}</p>
      ) : state ? (
        <>
          <WorkerPoolInventory
            canManage={canMutatePlacement}
            onUpdated={(next) => {
              queryClient.setQueryData<ControlPlaneExecutionPlacementState>(queryKey, (current) =>
                current
                  ? {
                      ...current,
                      pools: current.pools.map((pool) => (pool.id === next.id ? next : pool)),
                    }
                  : current,
              );
            }}
            state={state}
            targetId={props.target.id}
            tenantId={props.tenantId}
          />
          {canMutatePlacement ? (
            <>
              <PlacementPolicyEditor
                disabled={updatePolicy.isPending}
                onChange={updateMapping}
                state={state}
              />
              <CreateWarmPoolForm
                onCreated={() => queryClient.invalidateQueries({ queryKey })}
                targetId={props.target.id}
                tenantId={props.tenantId}
              />
            </>
          ) : null}
          {updatePolicy.error ? (
            <p className="text-destructive">{errorMessage(updatePolicy.error)}</p>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function WorkerPoolInventory(props: {
  state: ControlPlaneExecutionPlacementState;
  tenantId: string;
  targetId: string;
  canManage: boolean;
  onUpdated: (pool: ControlPlaneWorkerPool) => void;
}) {
  return (
    <div className="grid gap-1.5">
      {props.state.pools.map((pool) => (
        <WorkerPoolInventoryRow
          key={`${pool.id}:${pool.version}`}
          canManage={props.canManage}
          onUpdated={props.onUpdated}
          pool={pool}
          targetId={props.targetId}
          tenantId={props.tenantId}
        />
      ))}
    </div>
  );
}

function WorkerPoolInventoryRow(props: {
  pool: ControlPlaneWorkerPool;
  tenantId: string;
  targetId: string;
  canManage: boolean;
  onUpdated: (pool: ControlPlaneWorkerPool) => void;
}) {
  const [desiredIdleUnits, setDesiredIdleUnits] = useState(String(props.pool.desiredIdleUnits));
  const [maxActiveUnits, setMaxActiveUnits] = useState(String(props.pool.maxActiveUnits));
  const [status, setStatus] = useState<ControlPlaneWorkerPoolStatus>(props.pool.status);
  const updatePool = useMutation({
    mutationFn: () =>
      controlPlaneClient.updateWorkerPool(
        props.tenantId,
        props.targetId,
        props.pool.id,
        buildWorkerPoolUpdateInput(props.pool, {
          desiredIdleUnits,
          maxActiveUnits,
          status,
        }),
      ),
    onSuccess: props.onUpdated,
  });
  const desired = Number(desiredIdleUnits);
  const maximum = Number(maxActiveUnits);
  const validCapacity =
    Number.isSafeInteger(desired) &&
    Number.isSafeInteger(maximum) &&
    desired >= 0 &&
    maximum >= desired;
  const unchanged =
    desired === props.pool.desiredIdleUnits &&
    maximum === props.pool.maxActiveUnits &&
    status === props.pool.status;

  return (
    <div className="space-y-2 rounded-md border border-border/80 bg-background/70 px-2.5 py-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span>
          <span className="font-medium text-foreground">{props.pool.name}</span>
          <span className="ml-1.5 text-muted-foreground">
            {props.pool.capacityClass} · idle {props.pool.desiredIdleUnits} · max{" "}
            {props.pool.maxActiveUnits} · {props.pool.tenantIsolation} · v{props.pool.version}
          </span>
        </span>
        <span className="flex flex-wrap gap-1">
          <ControlPlaneStatusPill active={false} value={props.pool.mode} />
          <ControlPlaneStatusPill active={false} value={props.pool.tenantIsolation} />
          <ControlPlaneStatusPill value={props.pool.status} />
        </span>
      </div>
      {props.canManage ? (
        <form
          className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1.3fr)_auto]"
          onSubmit={(event) => {
            event.preventDefault();
            if (validCapacity && !unchanged) updatePool.mutate();
          }}
        >
          <ControlPlaneFormField label="Desired idle">
            <Input
              min={0}
              required
              step={1}
              type="number"
              value={desiredIdleUnits}
              onChange={(event) => setDesiredIdleUnits(event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Maximum units">
            <Input
              min={Math.max(1, Number.isFinite(desired) ? desired : 1)}
              required
              step={1}
              type="number"
              value={maxActiveUnits}
              onChange={(event) => setMaxActiveUnits(event.target.value)}
            />
          </ControlPlaneFormField>
          <ControlPlaneFormField label="Pool state">
            <select
              className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
              value={status}
              onChange={(event) => setStatus(event.target.value as ControlPlaneWorkerPoolStatus)}
            >
              <option value="active">Active</option>
              <option value="draining">Draining</option>
              <option value="disabled">Disabled</option>
            </select>
          </ControlPlaneFormField>
          <span className="self-end">
            <Button
              disabled={!validCapacity || unchanged || updatePool.isPending}
              size="xs"
              type="submit"
            >
              {updatePool.isPending ? "Saving…" : "Save capacity"}
            </Button>
          </span>
          {updatePool.error ? (
            <p className="text-destructive sm:col-span-4">{errorMessage(updatePool.error)}</p>
          ) : null}
        </form>
      ) : null}
    </div>
  );
}

function PlacementPolicyEditor(props: {
  state: ControlPlaneExecutionPlacementState;
  disabled: boolean;
  onChange: (field: "defaultPoolId" | "balancedPoolId" | "lowLatencyPoolId", value: string) => void;
}) {
  const activePools = props.state.pools.filter((pool) => pool.status === "active");
  return (
    <div className={CONTROL_PLANE_FORM_GRID_CLASS_NAME}>
      <ControlPlaneFormField label="Default pool">
        <select
          className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
          disabled={props.disabled}
          value={props.state.policy.defaultPoolId}
          onChange={(event) => props.onChange("defaultPoolId", event.target.value)}
        >
          {activePools.map((pool) => (
            <option key={pool.id} value={pool.id}>
              {pool.name} · {pool.mode}
            </option>
          ))}
        </select>
      </ControlPlaneFormField>
      <ControlPlaneFormField label="Balanced preference">
        <PoolPreferenceSelect
          disabled={props.disabled}
          onChange={(value) => props.onChange("balancedPoolId", value)}
          pools={activePools}
          value={props.state.policy.balancedPoolId ?? ""}
        />
      </ControlPlaneFormField>
      <ControlPlaneFormField label="Low-latency preference">
        <PoolPreferenceSelect
          disabled={props.disabled}
          onChange={(value) => props.onChange("lowLatencyPoolId", value)}
          pools={activePools}
          value={props.state.policy.lowLatencyPoolId ?? ""}
        />
      </ControlPlaneFormField>
      <p className="self-end text-muted-foreground">Policy v{props.state.policy.version}</p>
    </div>
  );
}

function PoolPreferenceSelect(props: {
  pools: ControlPlaneExecutionPlacementState["pools"];
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  return (
    <select
      className={CONTROL_PLANE_NATIVE_SELECT_CLASS_NAME}
      disabled={props.disabled}
      value={props.value}
      onChange={(event) => props.onChange(event.target.value)}
    >
      <option value="">Inherit default</option>
      {props.pools.map((pool) => (
        <option key={pool.id} value={pool.id}>
          {pool.name} · {pool.mode}
        </option>
      ))}
    </select>
  );
}

function CreateWarmPoolForm(props: {
  tenantId: string;
  targetId: string;
  onCreated: () => void | Promise<unknown>;
}) {
  const [name, setName] = useState("interactive-warm");
  const [desiredIdleUnits, setDesiredIdleUnits] = useState("1");
  const [maxActiveUnits, setMaxActiveUnits] = useState("4");
  const createPool = useMutation({
    mutationFn: () =>
      controlPlaneClient.createWorkerPool(
        props.tenantId,
        props.targetId,
        buildWarmPoolInput({ name, desiredIdleUnits, maxActiveUnits }),
      ),
    onSuccess: () => props.onCreated(),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    createPool.mutate();
  };

  return (
    <form className={CONTROL_PLANE_FORM_GRID_CLASS_NAME} onSubmit={submit}>
      <ControlPlaneFormField label="New warm pool">
        <Input required value={name} onChange={(event) => setName(event.target.value)} />
      </ControlPlaneFormField>
      <ControlPlaneFormField label="Desired idle">
        <Input
          min={0}
          required
          type="number"
          value={desiredIdleUnits}
          onChange={(event) => setDesiredIdleUnits(event.target.value)}
        />
      </ControlPlaneFormField>
      <ControlPlaneFormField label="Maximum units">
        <Input
          min={1}
          required
          type="number"
          value={maxActiveUnits}
          onChange={(event) => setMaxActiveUnits(event.target.value)}
        />
      </ControlPlaneFormField>
      <span className="self-end">
        <Button disabled={createPool.isPending} size="xs" type="submit">
          {createPool.isPending ? "Creating…" : "Create warm pool"}
        </Button>
      </span>
      {createPool.error ? (
        <p className="text-destructive sm:col-span-2">{errorMessage(createPool.error)}</p>
      ) : null}
    </form>
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The placement request failed.";
}
