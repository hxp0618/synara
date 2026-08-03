// FILE: TenantDataResidencySettingsSection.tsx
// Purpose: Configure the execution Region boundary and download its explicit residency statement.
// Layer: Settings UI component

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  controlPlaneClient,
  type ControlPlaneDataResidencyStatement,
  type ControlPlaneExecutionSchedulingPolicy,
} from "@synara/control-plane-client";

import { useEnterpriseUiHost } from "./EnterpriseUiHost";

export function residencyQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "execution-scheduling-policy"] as const;
}

export function residencyStatementQueryKey(tenantId: string) {
  return ["control-plane", "tenants", tenantId, "data-residency-statement"] as const;
}

export function TenantDataResidencySettingsSection(props: {
  tenantId: string;
  tenantName: string;
  homeRegion: string;
  canManage: boolean;
}) {
  const { InlineError, SettingsListRow, SettingsSection } = useEnterpriseUiHost();
  const policy = useQuery({
    queryKey: residencyQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantExecutionSchedulingPolicy(props.tenantId),
    retry: false,
  });
  const statement = useQuery({
    queryKey: residencyStatementQueryKey(props.tenantId),
    queryFn: () => controlPlaneClient.getTenantDataResidencyStatementDownload(props.tenantId),
    retry: false,
  });
  if (policy.isPending) {
    return (
      <SettingsSection title="Data residency">
        <SettingsListRow title="Loading execution Region boundary…" />
      </SettingsSection>
    );
  }
  if (policy.data) {
    return (
      <ResidencyPolicy
        key={policy.data.scope.version}
        {...props}
        policy={policy.data}
        statement={statement.data?.statement ?? null}
        statementBody={statement.data?.body ?? null}
        statementError={statement.error}
        statementPending={statement.isPending}
      />
    );
  }
  return (
    <SettingsSection title="Data residency">
      <SettingsListRow title="Execution Region boundary unavailable" />
      <InlineError error={policy.error} />
    </SettingsSection>
  );
}

function ResidencyPolicy(props: {
  tenantId: string;
  tenantName: string;
  homeRegion: string;
  canManage: boolean;
  policy: ControlPlaneExecutionSchedulingPolicy;
  statement: ControlPlaneDataResidencyStatement | null;
  statementBody: string | null;
  statementError: unknown;
  statementPending: boolean;
}) {
  const {
    Button,
    InlineError,
    Input,
    SettingsListRow,
    SettingsRow,
    SettingsSection,
    StatusPill,
    downloadJsonFile,
  } = useEnterpriseUiHost();
  const queryClient = useQueryClient();
  const currentRule = props.policy.scope.document.region;
  const deniedByPolicy = props.policy.scope.document.denyAll;
  const [regionsText, setRegionsText] = useState(
    currentRule.mode === "allow" ? currentRule.values.join(", ") : props.homeRegion,
  );
  const regions = normalizeRegions(regionsText);
  const update = useMutation({
    mutationFn: () =>
      controlPlaneClient.updateTenantExecutionSchedulingPolicy(props.tenantId, {
        expectedVersion: props.policy.scope.version,
        document: {
          ...props.policy.scope.document,
          region: { mode: "allow", values: regions },
        },
      }),
    onSuccess: (result) => {
      queryClient.setQueryData(residencyQueryKey(props.tenantId), result);
      void queryClient.invalidateQueries({
        queryKey: residencyStatementQueryKey(props.tenantId),
      });
    },
  });
  const enforced = deniedByPolicy || currentRule.mode === "allow";
  const statementMatchesPolicy =
    props.statement !== null &&
    props.statement.schemaVersion === "synara-data-residency-statement-v1" &&
    props.statement.execution.policyVersion === props.policy.scope.version &&
    props.statement.execution.policyDigest === props.policy.scope.digest &&
    props.statementBody !== null;

  return (
    <SettingsSection title="Data residency">
      <SettingsListRow
        title="Execution Region boundary"
        description={
          deniedByPolicy
            ? "Execution is denied by the Tenant policy. No target can be selected until an authorized administrator changes the policy."
            : enforced
              ? `Execution placement is restricted to ${currentRule.values.join(", ")}. Candidate selection and commit both verify the versioned policy.`
              : "Execution placement is currently unrestricted. The Tenant home Region is a label, not a residency promise, until an allow-list is enforced."
        }
        actions={
          <StatusPill active={enforced} value={enforced ? "execution enforced" : "unrestricted"} />
        }
      />
      <SettingsListRow
        title={`Home Region · ${props.homeRegion}`}
        description="Metadata, Artifact and KMS residency require a signed deployment annex; this execution policy alone does not make those data-plane promises."
      />
      {props.canManage ? (
        <SettingsRow
          title="Allowed execution Regions"
          description="Comma-separated Region codes. Cross-Region routing and evacuation remain fail-closed outside this list. Include the home Region before saving."
        >
          <div className="grid gap-2">
            <Input
              aria-label="Allowed execution Regions"
              placeholder="cn-east-1, cn-north-1"
              value={regionsText}
              onChange={(event) => setRegionsText(event.target.value)}
            />
            <Button
              disabled={
                update.isPending || regions.length === 0 || !regions.includes(props.homeRegion)
              }
              size="sm"
              type="button"
              onClick={() => update.mutate()}
            >
              {update.isPending ? "Enforcing…" : "Enforce execution Regions"}
            </Button>
            <InlineError error={update.error} />
          </div>
        </SettingsRow>
      ) : null}
      <SettingsRow
        title="Written boundary statement"
        description="Download the current policy version and digest. An unrestricted statement or missing deployment annex must not be represented as full data residency."
      >
        <Button
          size="sm"
          type="button"
          variant="outline"
          disabled={props.statementPending || !statementMatchesPolicy}
          onClick={() =>
            props.statement && props.statementBody
              ? downloadJsonFile(
                  `synara-data-residency-${props.tenantId}-v${props.statement.execution.policyVersion}.json`,
                  props.statementBody,
                )
              : undefined
          }
        >
          {props.statementPending ? "Preparing statement…" : "Download statement"}
        </Button>
        {!props.statementPending && !statementMatchesPolicy ? (
          <p className="text-xs text-muted-foreground">
            The server statement is unavailable or reflects an older policy. Refresh before
            downloading.
          </p>
        ) : null}
        <InlineError error={props.statementError} />
      </SettingsRow>
    </SettingsSection>
  );
}

function normalizeRegions(value: string): string[] {
  return [
    ...new Set(
      value
        .split(",")
        .map((region) => region.trim())
        .filter(Boolean),
    ),
  ].sort();
}

export function buildResidencyStatement(props: {
  tenantId: string;
  tenantName: string;
  homeRegion: string;
  policy: ControlPlaneExecutionSchedulingPolicy;
}) {
  const rule = props.policy.scope.document.region;
  const deniedByPolicy = props.policy.scope.document.denyAll;
  return {
    schemaVersion: "synara-data-residency-statement-v1",
    generatedAt: new Date().toISOString(),
    tenant: { id: props.tenantId, name: props.tenantName, homeRegion: props.homeRegion },
    execution: {
      status: deniedByPolicy || rule.mode === "allow" ? "enforced" : "unrestricted",
      allowedRegions: rule.mode === "allow" ? rule.values : [],
      policyVersion: props.policy.scope.version,
      policyDigest: props.policy.scope.digest,
      enforcement: "candidate-selection-and-commit",
    },
    dataPlanes: {
      metadata: "deployment-annex-required",
      artifacts: "deployment-annex-required",
      kms: "deployment-annex-required",
    },
    limitation:
      "This statement is not a full data-residency promise unless execution is enforced and a signed deployment annex covers Metadata, Artifact, KMS, backup, log and support-processing Regions.",
  };
}
