import "../../index.css";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { page } from "vitest/browser";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";

import { WorkerReleaseControls, workerReleasesQueryKey } from "./WorkerReleaseControls";
import {
  ControlPlaneError,
  controlPlaneClient,
  type ControlPlaneExecutionTarget,
  type ControlPlaneWorkerManifest,
  type ControlPlaneWorkerReleaseOverview,
  type ControlPlaneWorkerReleasePolicy,
} from "~/lib/controlPlaneClient";

const activeTarget: ControlPlaneExecutionTarget = {
  id: "target-1",
  tenantId: "tenant-1",
  organizationId: "organization-1",
  kind: "docker",
  name: "Docker workers",
  status: "active",
  capabilities: {},
  createdAt: "2026-07-15T00:00:00Z",
  updatedAt: "2026-07-15T00:00:00Z",
};

const manifests: ReadonlyArray<ControlPlaneWorkerManifest> = [
  workerManifest("manifest-1", "0.6.0", "a"),
  workerManifest("manifest-2", "0.7.0", "b"),
];

const baselinePolicy: ControlPlaneWorkerReleasePolicy = {
  tenantId: "tenant-1",
  executionTargetId: "target-1",
  policyVersion: 2,
  promotedRevisionId: "release-1",
  canaryPercent: 0,
  updatedBy: "user-1",
  updatedAt: "2026-07-15T01:00:00Z",
};

function workerManifest(
  manifestId: string,
  version: string,
  digestCharacter: string,
): ControlPlaneWorkerManifest {
  return {
    executionTargetId: activeTarget.id,
    manifestId,
    workerStatusCounts: { online: 1, draining: 0, offline: 0 },
    lastHeartbeatAt: "2026-07-15T01:00:00Z",
    workerBuild: {
      version,
      imageDigest: `sha256:${digestCharacter.repeat(64)}`,
      operatingSystem: "linux",
      architecture: "amd64",
    },
    workerProtocol: { minimum: 2, maximum: 2 },
    runtimeEvent: { minimum: 2, maximum: 2 },
    providers: [],
  };
}

function releaseOverview(
  policy: ControlPlaneWorkerReleaseOverview["policy"],
): ControlPlaneWorkerReleaseOverview {
  return {
    policy,
    revisions: [
      {
        id: "release-2",
        tenantId: "tenant-1",
        executionTargetId: "target-1",
        revision: 2,
        workerManifestId: "manifest-2",
        workerBuildVersion: "0.7.0",
        imageDigest: `sha256:${"b".repeat(64)}`,
        description: "Candidate",
        createdBy: "user-1",
        createdAt: "2026-07-15T01:00:00Z",
      },
      {
        id: "release-1",
        tenantId: "tenant-1",
        executionTargetId: "target-1",
        revision: 1,
        workerManifestId: "manifest-1",
        workerBuildVersion: "0.6.0",
        imageDigest: `sha256:${"a".repeat(64)}`,
        description: "Baseline",
        createdBy: "user-1",
        createdAt: "2026-07-15T00:00:00Z",
      },
    ],
    transitions: [],
  };
}

async function mountWorkerReleaseControls(input?: {
  target?: ControlPlaneExecutionTarget;
  overview?: ControlPlaneWorkerReleaseOverview;
}) {
  const target = input?.target ?? activeTarget;
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  if (input?.overview) {
    queryClient.setQueryData(workerReleasesQueryKey("tenant-1", target.id), input.overview);
  }
  const host = document.createElement("div");
  document.body.append(host);
  const screen = await render(
    <QueryClientProvider client={queryClient}>
      <WorkerReleaseControls
        canManage
        enabled
        manifests={manifests}
        target={target}
        tenantId="tenant-1"
      />
    </QueryClientProvider>,
    { container: host },
  );
  const cleanup = async () => {
    await screen.unmount();
    queryClient.clear();
    host.remove();
  };
  return { [Symbol.asyncDispose]: cleanup, cleanup, queryClient };
}

describe("WorkerReleaseControls interactions", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  it("refreshes the overview after a CAS conflict and asks the operator to review it", async () => {
    const refreshedPolicy = {
      ...baselinePolicy,
      policyVersion: 3,
      canaryRevisionId: "release-2",
      canaryPercent: 10,
    } satisfies ControlPlaneWorkerReleasePolicy;
    const listReleases = vi
      .spyOn(controlPlaneClient, "listWorkerReleases")
      .mockResolvedValueOnce(releaseOverview(baselinePolicy))
      .mockResolvedValueOnce(releaseOverview(refreshedPolicy));
    const transition = vi
      .spyOn(controlPlaneClient, "transitionWorkerRelease")
      .mockRejectedValue(
        new ControlPlaneError(
          409,
          "worker_release_policy_version_conflict",
          "Worker release policy changed before this transition could be committed.",
          "request-cas",
          { currentPolicyVersion: 3 },
        ),
      );

    await using _ = await mountWorkerReleaseControls();

    await expect.element(page.getByText("policy v2")).toBeInTheDocument();
    await page.getByLabelText("Release reason").fill("Start the candidate rollout");
    await page.getByRole("button", { name: "Start canary with revision 2" }).click();

    await expect.poll(() => listReleases.mock.calls.length).toBe(2);
    await expect.element(page.getByText("policy v3")).toBeInTheDocument();
    await expect
      .element(
        page.getByText(
          "The Worker release policy changed to version 3 and has been refreshed. Review the current policy before retrying.",
        ),
      )
      .toBeInTheDocument();
    expect(transition).toHaveBeenCalledWith(
      "tenant-1",
      "target-1",
      "release-2",
      "canary",
      { expectedPolicyVersion: 2, reason: "Start the candidate rollout", canaryPercent: 10 },
      { idempotencyKey: expect.stringMatching(/^web-worker-release-policy-/) },
    );
  });

  it("aborts a canary by rolling back to the still-promoted revision", async () => {
    const canaryPolicy = {
      ...baselinePolicy,
      canaryRevisionId: "release-2",
      canaryPercent: 10,
    } satisfies ControlPlaneWorkerReleasePolicy;
    vi.spyOn(controlPlaneClient, "listWorkerReleases").mockResolvedValue(
      releaseOverview({ ...baselinePolicy, policyVersion: 3 }),
    );
    const transition = vi
      .spyOn(controlPlaneClient, "transitionWorkerRelease")
      .mockResolvedValue({ ...baselinePolicy, policyVersion: 3 });

    await using _ = await mountWorkerReleaseControls({ overview: releaseOverview(canaryPolicy) });

    await page.getByLabelText("Release reason").fill("Candidate failed the health gate");
    await page
      .getByRole("button", {
        name: "Abort canary revision 2 and keep promoted revision 1",
      })
      .click();

    await expect.poll(() => transition.mock.calls.length).toBe(1);
    expect(transition).toHaveBeenCalledWith(
      "tenant-1",
      "target-1",
      "release-1",
      "rollback",
      { expectedPolicyVersion: 2, reason: "Candidate failed the health gate" },
      { idempotencyKey: expect.stringMatching(/^web-worker-release-policy-/) },
    );
  });

  it("keeps release mutations disabled while the Execution Target is inactive", async () => {
    const target = { ...activeTarget, status: "offline" as const };
    const transition = vi.spyOn(controlPlaneClient, "transitionWorkerRelease");

    await using _ = await mountWorkerReleaseControls({
      target,
      overview: releaseOverview(null),
    });

    await expect
      .element(
        page.getByText(/Release changes are disabled while this Execution Target is offline/),
      )
      .toBeInTheDocument();
    const reasonInput = document.querySelector<HTMLInputElement>(
      'input[placeholder="Why this rollout or rollback is required"]',
    );
    const promoteButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Promote revision 1 as baseline"]',
    );
    const registerButton = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Register immutable revision",
    );

    expect(reasonInput?.disabled).toBe(true);
    expect(promoteButton?.disabled).toBe(true);
    expect(registerButton?.disabled).toBe(true);
    expect(transition).not.toHaveBeenCalled();
  });
});
