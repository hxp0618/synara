// FILE: controlPlaneArtifacts.ts
// Purpose: Derive user-facing SaaS Artifact state from durable Session events and metadata.
// Layer: Web Control Plane projection helpers
// Exports: Artifact sequence, filtering, naming, and kind labels

import type { OrchestrationThreadActivity } from "@synara/contracts";

import type { ControlPlaneArtifact } from "./controlPlaneClient";

const USER_DOWNLOADABLE_ARTIFACT_KINDS = new Set<ControlPlaneArtifact["kind"]>([
  "attachment",
  "generated_file",
  "terminal_log",
  "diff",
]);

const ARTIFACT_KIND_LABELS: Record<ControlPlaneArtifact["kind"], string> = {
  attachment: "Attachment",
  generated_file: "Generated file",
  terminal_log: "Terminal log",
  diff: "Diff",
  workspace_snapshot: "Workspace snapshot",
  checkpoint: "Checkpoint",
};

export function latestControlPlaneArtifactReadySequence(
  activities: ReadonlyArray<OrchestrationThreadActivity>,
): number | null {
  let latest: number | null = null;
  for (const activity of activities) {
    if (activity.kind !== "artifact.ready" || activity.sequence === undefined) continue;
    latest = latest === null ? activity.sequence : Math.max(latest, activity.sequence);
  }
  return latest;
}

export function userDownloadableControlPlaneArtifacts(
  artifacts: ReadonlyArray<ControlPlaneArtifact>,
): ReadonlyArray<ControlPlaneArtifact> {
  return artifacts
    .filter(
      (artifact) =>
        artifact.status === "ready" &&
        artifact.deletedAt === null &&
        USER_DOWNLOADABLE_ARTIFACT_KINDS.has(artifact.kind),
    )
    .toSorted((left, right) => {
      const timeComparison = (right.readyAt ?? right.createdAt).localeCompare(
        left.readyAt ?? left.createdAt,
      );
      return timeComparison === 0 ? left.id.localeCompare(right.id) : timeComparison;
    });
}

export function controlPlaneArtifactKindLabel(kind: ControlPlaneArtifact["kind"]): string {
  return ARTIFACT_KIND_LABELS[kind];
}

export function controlPlaneArtifactDisplayName(artifact: ControlPlaneArtifact): string {
  const normalized = artifact.originalName?.trim().replaceAll("\\", "/");
  const leafName = normalized?.split("/").at(-1)?.trim();
  return leafName || `${controlPlaneArtifactKindLabel(artifact.kind)} ${artifact.id.slice(0, 8)}`;
}
