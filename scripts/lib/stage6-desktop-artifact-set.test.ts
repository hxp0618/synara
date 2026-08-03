import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  assertVerifiedStage6DesktopArtifactSet,
  computeStage6DesktopArtifactSetSha256,
  verifyStage6DesktopArtifactSet,
} from "./stage6-desktop-artifact-set.ts";

const temporaryDirectories: string[] = [];
const sourceCommit = "a".repeat(40);
const lockfileSha256 = "b".repeat(64);
const candidateTag = "v1.2.3";
const targets = {
  "linux-x64": ["linux", "x64", "AppImage", "Synara-1.2.3-x64.AppImage"],
  "macos-arm64": ["mac", "arm64", "dmg", "Synara-1.2.3-arm64.dmg"],
  "macos-x64": ["mac", "x64", "dmg", "Synara-1.2.3-x64.dmg"],
  "windows-x64": ["win", "x64", "nsis", "Synara-1.2.3-x64.exe"],
} as const;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

function sha256(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

function createArtifactSet(): {
  readonly assetsDirectory: string;
  readonly expectedArtifactSetSha256: string;
} {
  const root = mkdtempSync(join(tmpdir(), "synara-stage6-artifact-set-"));
  temporaryDirectories.push(root);
  const evidenceDigests = {} as Record<
    keyof typeof targets,
    {
      provenance: string;
      attestationBundle: string;
      artifactAttestation: string;
    }
  >;
  for (const [targetId, [platform, arch, target, fileName]] of Object.entries(targets) as Array<
    [keyof typeof targets, (typeof targets)[keyof typeof targets]]
  >) {
    const bytes = `${targetId}-payload`;
    writeFileSync(join(root, fileName), bytes);
    const provenance = {
      schemaVersion: 1,
      publication: true,
      platform,
      arch,
      target,
      version: "1.2.3",
      source: {
        commit: sourceCommit,
        tag: candidateTag,
        lockfileSha256,
      },
      signing: { status: "fixture" },
      artifacts: [{ fileName, size: Buffer.byteLength(bytes), sha256: sha256(bytes) }],
    };
    const provenanceFile = `artifact-${platform}-${arch}.provenance.json`;
    const encoded = `${JSON.stringify(provenance, null, 2)}\n`;
    writeFileSync(join(root, provenanceFile), encoded);
    const attestationBundle = `${targetId}-signed-attestation-bundle`;
    const artifactAttestation = `${targetId}-verified-primary-installer-attestation`;
    writeFileSync(join(root, `artifact-${platform}-${arch}.attestation.json`), attestationBundle);
    writeFileSync(
      join(root, `artifact-${platform}-${arch}.attestation-verification.json`),
      artifactAttestation,
    );
    evidenceDigests[targetId] = {
      provenance: `sha256:${sha256(encoded)}`,
      attestationBundle: `sha256:${sha256(attestationBundle)}`,
      artifactAttestation: `sha256:${sha256(artifactAttestation)}`,
    };
  }
  return {
    assetsDirectory: root,
    expectedArtifactSetSha256: computeStage6DesktopArtifactSetSha256(evidenceDigests),
  };
}

describe("Stage 6 Desktop exact artifact set", () => {
  it("binds all four provenance manifests and their payload bytes", () => {
    const fixture = createArtifactSet();
    const result = verifyStage6DesktopArtifactSet({
      ...fixture,
      candidateTag,
      sourceCommit,
      lockfileSha256,
    });
    expect(result.targets).toHaveLength(4);
    expect(result.targets.map((target) => target.id)).toEqual([
      "linux-x64",
      "macos-arm64",
      "macos-x64",
      "windows-x64",
    ]);
    expect(result.targets.every((target) => target.artifacts.length === 1)).toBe(true);
    expect(() => assertVerifiedStage6DesktopArtifactSet(result)).not.toThrow();
    expect(() => assertVerifiedStage6DesktopArtifactSet({ ...result })).toThrow(
      "was not produced by the local byte verifier",
    );
  });

  it("rejects a payload changed after provenance was written", () => {
    const fixture = createArtifactSet();
    writeFileSync(join(fixture.assetsDirectory, "Synara-1.2.3-arm64.dmg"), "different");
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit,
        lockfileSha256,
      }),
    ).toThrow("does not match its provenance");
  });

  it("rejects a provenance set from another candidate source", () => {
    const fixture = createArtifactSet();
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit: "c".repeat(40),
        lockfileSha256,
      }),
    ).toThrow("source does not match");
  });

  it("rejects an artifact-set digest not approved by candidate evidence", () => {
    const fixture = createArtifactSet();
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit,
        lockfileSha256,
        expectedArtifactSetSha256: `sha256:${"d".repeat(64)}`,
      }),
    ).toThrow("does not match the approved candidate evidence");
  });

  it("rejects attestation evidence mixed in after candidate review", () => {
    const fixture = createArtifactSet();
    writeFileSync(
      join(fixture.assetsDirectory, "artifact-win-x64.attestation.json"),
      "different-attestation",
    );
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit,
        lockfileSha256,
      }),
    ).toThrow("does not match the approved candidate evidence");
  });

  it("rejects a payload that is not named by any approved provenance manifest", () => {
    const fixture = createArtifactSet();
    writeFileSync(join(fixture.assetsDirectory, "unexpected.zip"), "unprovenanced");
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit,
        lockfileSha256,
      }),
    ).toThrow("Unprovenanced Stage 6 Desktop payload");
  });

  it("leaves raw updater metadata to the finalized public asset manifest", () => {
    const fixture = createArtifactSet();
    writeFileSync(join(fixture.assetsDirectory, "latest-mac.yml"), "version: 1.2.3\n");
    expect(() =>
      verifyStage6DesktopArtifactSet({
        ...fixture,
        candidateTag,
        sourceCommit,
        lockfileSha256,
      }),
    ).not.toThrow();
  });
});
