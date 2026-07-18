import type { OrchestrationThreadActivity } from "@synara/contracts";
import { describe, expect, it } from "vitest";

import type { ControlPlaneArtifact } from "./controlPlaneClient";
import {
  controlPlaneArtifactDisplayName,
  latestControlPlaneArtifactReadySequence,
  userDownloadableControlPlaneArtifacts,
} from "./controlPlaneArtifacts";

function artifact(
  input: Partial<ControlPlaneArtifact> & Pick<ControlPlaneArtifact, "id" | "kind" | "status">,
): ControlPlaneArtifact {
  return {
    id: input.id,
    tenantId: "tenant-1",
    organizationId: "organization-1",
    projectId: "project-1",
    sessionId: "session-1",
    executionId: null,
    kind: input.kind,
    status: input.status,
    originalName: input.originalName ?? null,
    contentType: input.contentType ?? null,
    sizeBytes: input.sizeBytes ?? null,
    sha256: input.sha256 ?? null,
    createdByType: "worker",
    createdById: "worker-1",
    readyAt: input.readyAt ?? null,
    createdAt: input.createdAt ?? "2026-07-18T00:00:00.000Z",
    expiresAt: null,
    deletedAt: input.deletedAt ?? null,
  };
}

function activity(kind: string, sequence: number): OrchestrationThreadActivity {
  return {
    id: `event-${sequence}`,
    tone: "info",
    kind,
    summary: kind,
    payload: {},
    turnId: null,
    sequence,
    createdAt: "2026-07-18T00:00:00.000Z",
  } as OrchestrationThreadActivity;
}

describe("controlPlaneArtifacts", () => {
  it("tracks only the latest durable artifact.ready sequence", () => {
    expect(
      latestControlPlaneArtifactReadySequence([
        activity("artifact.ready", 4),
        activity("execution.completed", 9),
        activity("artifact.ready", 7),
      ]),
    ).toBe(7);
  });

  it("keeps ready user artifacts newest-first and excludes internal or deleted entries", () => {
    const visible = userDownloadableControlPlaneArtifacts([
      artifact({
        id: "artifact-old",
        kind: "generated_file",
        status: "ready",
        readyAt: "2026-07-18T00:00:01.000Z",
      }),
      artifact({
        id: "artifact-new",
        kind: "diff",
        status: "ready",
        readyAt: "2026-07-18T00:00:03.000Z",
      }),
      artifact({ id: "artifact-pending", kind: "terminal_log", status: "pending" }),
      artifact({ id: "artifact-checkpoint", kind: "checkpoint", status: "ready" }),
      artifact({
        id: "artifact-deleted",
        kind: "attachment",
        status: "ready",
        deletedAt: "2026-07-18T00:00:04.000Z",
      }),
    ]);

    expect(visible.map((item) => item.id)).toEqual(["artifact-new", "artifact-old"]);
  });

  it("uses a safe leaf display name and a stable fallback", () => {
    expect(
      controlPlaneArtifactDisplayName(
        artifact({
          id: "8606c570-1a52-4b64-8445-a4198ff08ca0",
          kind: "generated_file",
          status: "ready",
          originalName: "nested\\report.txt",
        }),
      ),
    ).toBe("report.txt");
    expect(
      controlPlaneArtifactDisplayName(
        artifact({
          id: "8606c570-1a52-4b64-8445-a4198ff08ca0",
          kind: "diff",
          status: "ready",
        }),
      ),
    ).toBe("Diff 8606c570");
  });
});
