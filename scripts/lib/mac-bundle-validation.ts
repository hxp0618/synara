// FILE: mac-bundle-validation.ts
// Purpose: Reject mixed-architecture or mixed-Team-ID macOS app bundles before distribution.
// Layer: Release/build helper

import { spawnSync } from "node:child_process";
import { closeSync, lstatSync, openSync, readSync, readdirSync } from "node:fs";
import { relative, resolve } from "node:path";

export type MacArtifactArch = "arm64" | "x64" | "universal";

export interface MacBinaryArchitecture {
  readonly path: string;
  readonly architectures: ReadonlyArray<string>;
}

const MACH_O_MAGICS = new Set([0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe]);
const FAT_MAGICS = new Set([0xcafebabe, 0xcafebabf, 0xbebafeca, 0xbfbafeca]);

function isMachO(path: string): boolean {
  const descriptor = openSync(path, "r");
  try {
    const header = Buffer.allocUnsafe(4);
    if (readSync(descriptor, header, 0, header.length, 0) !== header.length) return false;
    const magic = header.readUInt32BE(0);
    return MACH_O_MAGICS.has(magic) || FAT_MAGICS.has(magic);
  } finally {
    closeSync(descriptor);
  }
}

export function collectMachOBinaries(appBundlePath: string): ReadonlyArray<string> {
  const root = resolve(appBundlePath);
  const pending = [root];
  const binaries: string[] = [];
  while (pending.length > 0) {
    const current = pending.pop();
    if (!current) continue;
    for (const entry of readdirSync(current)) {
      const candidate = resolve(current, entry);
      const stat = lstatSync(candidate);
      if (stat.isSymbolicLink()) continue;
      if (stat.isDirectory()) {
        pending.push(candidate);
      } else if (stat.isFile() && isMachO(candidate)) {
        binaries.push(candidate);
      }
    }
  }
  return binaries.sort((left, right) => left.localeCompare(right));
}

function run(command: string, args: ReadonlyArray<string>): string {
  const result = spawnSync(command, [...args], { encoding: "utf8" });
  const output = `${result.stdout ?? ""}\n${result.stderr ?? ""}`.trim();
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(" ")} failed: ${output}`);
  }
  return output;
}

export function validateMacBinaryArchitectures(
  binaries: ReadonlyArray<MacBinaryArchitecture>,
  expectedArch: MacArtifactArch,
): void {
  const required =
    expectedArch === "universal"
      ? ["arm64", "x86_64"]
      : [expectedArch === "x64" ? "x86_64" : expectedArch];
  const dependencyArchitectureGroups = new Map<string, Set<string>>();
  const architectureQualifiedDependencyPath = (binaryPath: string): string | null => {
    const dependencyPrefix = "Contents/Resources/app.asar.unpacked/node_modules/";
    if (!binaryPath.startsWith(dependencyPrefix)) return null;
    let qualified = false;
    const canonicalPath = binaryPath.replace(
      /(^|[-_/])(arm64|x64|x86_64)(?=$|[-_/.])/gu,
      (_match, separator: string) => {
        qualified = true;
        return `${separator}<arch>`;
      },
    );
    return qualified ? canonicalPath : null;
  };

  for (const binary of binaries) {
    const group = architectureQualifiedDependencyPath(binary.path);
    if (!group) continue;
    const architectures = dependencyArchitectureGroups.get(group) ?? new Set<string>();
    for (const architecture of binary.architectures) architectures.add(architecture);
    dependencyArchitectureGroups.set(group, architectures);
  }

  const failures = binaries.filter((binary) => {
    if (required.every((architecture) => binary.architectures.includes(architecture))) {
      return false;
    }
    const group = architectureQualifiedDependencyPath(binary.path);
    const groupArchitectures = group ? dependencyArchitectureGroups.get(group) : undefined;
    return required.some((architecture) => !groupArchitectures?.has(architecture));
  });
  if (failures.length === 0) return;
  const details = failures
    .slice(0, 12)
    .map((binary) => `${binary.path} [${binary.architectures.join(", ") || "unknown"}]`)
    .join("; ");
  throw new Error(
    `macOS ${expectedArch} artifact contains incompatible Mach-O components: ${details}`,
  );
}

export function verifyMacBundleArchitectures(
  appBundlePath: string,
  expectedArch: MacArtifactArch,
): ReadonlyArray<string> {
  const binaries = collectMachOBinaries(appBundlePath);
  if (binaries.length === 0) {
    throw new Error(`No Mach-O binaries found in ${appBundlePath}.`);
  }
  const inspected = binaries.map((path) => ({
    path: relative(appBundlePath, path),
    architectures: run("lipo", ["-archs", path]).split(/\s+/u).filter(Boolean),
  }));
  validateMacBinaryArchitectures(inspected, expectedArch);
  return binaries;
}

export function parseCodesignTeamId(output: string): string | null {
  const value = /^TeamIdentifier=(.+)$/mu.exec(output)?.[1]?.trim();
  return value && value !== "not set" ? value : null;
}

export function verifyMacBundleTeamIdentity(
  appBundlePath: string,
  binaries: ReadonlyArray<string>,
): string {
  const rootTeamId = parseCodesignTeamId(run("codesign", ["-dvvv", appBundlePath]));
  if (!rootTeamId) {
    throw new Error(`Signed macOS app has no TeamIdentifier: ${appBundlePath}`);
  }
  const mismatches: string[] = [];
  for (const binary of binaries) {
    const teamId = parseCodesignTeamId(run("codesign", ["-dvvv", binary]));
    if (teamId !== rootTeamId) {
      mismatches.push(`${relative(appBundlePath, binary)} [${teamId ?? "no TeamIdentifier"}]`);
    }
  }
  if (mismatches.length > 0) {
    throw new Error(
      `macOS app contains code not signed by Team ${rootTeamId}: ${mismatches.slice(0, 12).join("; ")}`,
    );
  }
  return rootTeamId;
}
