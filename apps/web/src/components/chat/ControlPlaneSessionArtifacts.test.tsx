import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import type { ControlPlaneArtifact } from "~/lib/controlPlaneClient";
import { ControlPlaneSessionArtifacts } from "./ControlPlaneSessionArtifacts";

function artifact(input: {
  id: string;
  kind: ControlPlaneArtifact["kind"];
  status: ControlPlaneArtifact["status"];
  originalName?: string;
  sizeBytes?: number;
}): ControlPlaneArtifact {
  return {
    id: input.id,
    tenantId: "tenant-1",
    organizationId: "organization-1",
    projectId: "project-1",
    sessionId: "session-1",
    executionId: "execution-1",
    kind: input.kind,
    status: input.status,
    originalName: input.originalName ?? null,
    contentType: "text/plain; charset=utf-8",
    sizeBytes: input.sizeBytes ?? null,
    sha256: "a".repeat(64),
    createdByType: "worker",
    createdById: "worker-1",
    readyAt: input.status === "ready" ? "2026-07-18T00:00:01.000Z" : null,
    createdAt: "2026-07-18T00:00:00.000Z",
    expiresAt: null,
    deletedAt: null,
  };
}

describe("ControlPlaneSessionArtifacts", () => {
  it("renders ready user files with download metadata and excludes internal artifacts", () => {
    const markup = renderToStaticMarkup(
      <ControlPlaneSessionArtifacts
        artifacts={[
          artifact({
            id: "artifact-report",
            kind: "generated_file",
            status: "ready",
            originalName: "report.txt",
            sizeBytes: 12,
          }),
          artifact({ id: "artifact-pending", kind: "terminal_log", status: "pending" }),
          artifact({ id: "artifact-checkpoint", kind: "checkpoint", status: "ready" }),
        ]}
        downloadingArtifactId={null}
        onDownload={() => undefined}
        onRetry={() => undefined}
      />,
    );

    expect(markup).toContain("Session artifacts");
    expect(markup).toContain("1 ready file");
    expect(markup).toContain("report.txt");
    expect(markup).toContain("Generated file · 12 B");
    expect(markup).toContain('aria-label="Download report.txt"');
    expect(markup).not.toContain("artifact-pending");
    expect(markup).not.toContain("artifact-checkpoint");
  });

  it("shows a retry action when the durable artifact list cannot be refreshed", () => {
    const markup = renderToStaticMarkup(
      <ControlPlaneSessionArtifacts
        error={new Error("offline")}
        downloadingArtifactId={null}
        onDownload={() => undefined}
        onRetry={() => undefined}
      />,
    );

    expect(markup).toContain("Artifacts unavailable");
    expect(markup).toContain("Retry");
    expect(markup).not.toContain('aria-hidden="true"');
  });

  it("prevents overlapping downloads while one artifact is in flight", () => {
    const markup = renderToStaticMarkup(
      <ControlPlaneSessionArtifacts
        artifacts={[
          artifact({
            id: "artifact-first",
            kind: "generated_file",
            status: "ready",
            originalName: "first.txt",
          }),
          artifact({
            id: "artifact-second",
            kind: "generated_file",
            status: "ready",
            originalName: "second.txt",
          }),
        ]}
        downloadingArtifactId="artifact-first"
        onDownload={() => undefined}
        onRetry={() => undefined}
      />,
    );

    expect(markup.match(/ disabled=""/g)).toHaveLength(2);
  });
});
