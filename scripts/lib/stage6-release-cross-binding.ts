import { createHash } from "node:crypto";

import {
  type VerifiedReleaseFinalAssetSet,
  assertVerifiedReleaseFinalAssetSet,
} from "./release-final-asset-set.ts";
import {
  type VerifiedStage6DesktopArtifactSet,
  assertVerifiedStage6DesktopArtifactSet,
} from "./stage6-desktop-artifact-set.ts";

const VERIFIED_STAGE6_RELEASE_CROSS_BINDING: unique symbol = Symbol(
  "VerifiedStage6ReleaseCrossBinding",
);
const VERIFIED_STAGE6_RELEASE_CROSS_BINDINGS = new WeakSet<object>();

export const STAGE6_RELEASE_CROSS_BINDING_SCHEMA =
  "synara.stage6-release-raw-final-cross-binding.v1";

export interface Stage6ReleaseCrossBoundFile {
  readonly fileName: string;
  readonly sizeBytes: number;
  readonly sha256: string;
  readonly kind: "payload" | "trust";
}

export interface VerifiedStage6ReleaseCrossBinding {
  readonly schemaVersion: typeof STAGE6_RELEASE_CROSS_BINDING_SCHEMA;
  readonly source: {
    readonly tag: string;
    readonly commit: string;
    readonly lockfileSha256: string;
  };
  readonly rawDesktopArtifactSetSha256: string;
  readonly finalAssetSetSha256: string;
  readonly files: ReadonlyArray<Stage6ReleaseCrossBoundFile>;
  readonly crossBindingSha256: string;
  readonly [VERIFIED_STAGE6_RELEASE_CROSS_BINDING]: true;
}

export function assertVerifiedStage6ReleaseCrossBinding(
  value: unknown,
): asserts value is VerifiedStage6ReleaseCrossBinding {
  if (
    typeof value !== "object" ||
    value === null ||
    !VERIFIED_STAGE6_RELEASE_CROSS_BINDINGS.has(value)
  ) {
    throw new Error(
      "Stage 6 raw-to-final release binding was not produced by the local byte verifier.",
    );
  }
}

function canonicalDigestInput(
  value: Omit<
    VerifiedStage6ReleaseCrossBinding,
    "crossBindingSha256" | typeof VERIFIED_STAGE6_RELEASE_CROSS_BINDING
  >,
): string {
  return JSON.stringify(value);
}

export function verifyStage6ReleaseCrossBinding(
  raw: VerifiedStage6DesktopArtifactSet,
  finalAssets: VerifiedReleaseFinalAssetSet,
): VerifiedStage6ReleaseCrossBinding {
  assertVerifiedStage6DesktopArtifactSet(raw);
  assertVerifiedReleaseFinalAssetSet(finalAssets);

  if (
    finalAssets.source.tag !== raw.candidateTag ||
    finalAssets.source.commit !== raw.sourceCommit ||
    finalAssets.source.lockfileSha256 !== raw.lockfileSha256
  ) {
    throw new Error("Stage 6 raw and final release assets do not identify the same source.");
  }

  const expected = new Map<
    string,
    { readonly sizeBytes?: number; readonly sha256: string; readonly kind: "payload" | "trust" }
  >();
  const addExpected = (
    fileName: string,
    sha256: string,
    kind: "payload" | "trust",
    sizeBytes?: number,
  ): void => {
    if (expected.has(fileName)) {
      throw new Error(`Stage 6 raw candidate claims duplicate public file ${fileName}.`);
    }
    expected.set(
      fileName,
      sizeBytes === undefined ? { sha256, kind } : { sizeBytes, sha256, kind },
    );
  };

  for (const target of raw.targets) {
    addExpected(target.provenanceFile, target.provenanceSha256, "trust");
    addExpected(target.attestationBundleFile, target.attestationBundleSha256, "trust");
    addExpected(target.artifactAttestationFile, target.artifactAttestationSha256, "trust");
    for (const artifact of target.artifacts) {
      addExpected(artifact.fileName, artifact.sha256, "payload", artifact.sizeBytes);
    }
  }

  const actual = new Map(finalAssets.files.map((file) => [file.fileName, file] as const));
  const unexpected = finalAssets.files.find(
    (file) => !file.fileName.endsWith(".yml") && !expected.has(file.fileName),
  );
  if (unexpected) {
    throw new Error(
      `Final release contains ${unexpected.fileName}, which was not in the approved raw Desktop candidate.`,
    );
  }

  const files: Stage6ReleaseCrossBoundFile[] = [];
  for (const [fileName, rawFile] of [...expected.entries()].toSorted(([left], [right]) =>
    left.localeCompare(right),
  )) {
    const finalFile = actual.get(fileName);
    if (!finalFile) {
      throw new Error(`Final release is missing approved raw Desktop file ${fileName}.`);
    }
    if (
      finalFile.sha256 !== rawFile.sha256 ||
      (rawFile.sizeBytes !== undefined && finalFile.sizeBytes !== rawFile.sizeBytes)
    ) {
      throw new Error(
        `Final release file ${fileName} does not match the approved raw candidate bytes.`,
      );
    }
    files.push(
      Object.freeze({
        fileName,
        sizeBytes: finalFile.sizeBytes,
        sha256: finalFile.sha256,
        kind: rawFile.kind,
      }),
    );
  }

  const canonical = {
    schemaVersion: STAGE6_RELEASE_CROSS_BINDING_SCHEMA,
    source: {
      tag: raw.candidateTag,
      commit: raw.sourceCommit,
      lockfileSha256: raw.lockfileSha256,
    },
    rawDesktopArtifactSetSha256: raw.artifactSetSha256,
    finalAssetSetSha256: finalAssets.finalAssetSetSha256,
    files: Object.freeze(files),
  } as const;
  const verified: VerifiedStage6ReleaseCrossBinding = Object.freeze({
    ...canonical,
    crossBindingSha256: `sha256:${createHash("sha256")
      .update(canonicalDigestInput(canonical))
      .digest("hex")}`,
    [VERIFIED_STAGE6_RELEASE_CROSS_BINDING]: true as const,
  });
  VERIFIED_STAGE6_RELEASE_CROSS_BINDINGS.add(verified);
  return verified;
}
