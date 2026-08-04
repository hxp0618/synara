import { createHash } from "node:crypto";
import {
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import { basename, join, resolve } from "node:path";

type JSONRecord = Record<string, unknown>;

export type PolarisReleasePreparation = {
  npmVersion: string;
  pythonVersion: string;
  minimumControlPlaneVersion: string;
  npmDirectory: string;
  sourceFiles: ReadonlyArray<{ path: string; sha256: string }>;
};

export function preparePolarisSDKRelease(
  repositoryRoot: string,
  outputRoot: string,
): PolarisReleasePreparation {
  const root = resolve(repositoryRoot);
  const output = resolve(outputRoot);
  if (existsSync(output)) {
    throw new Error("Polaris release output must not already exist.");
  }

  const npmSource = join(root, "packages", "polaris-sdk");
  const pythonSource = join(root, "packages", "polaris-python");
  const npmManifest = readJSON(join(npmSource, "package.json"));
  const npmVersion = requireString(npmManifest.version, "npm SDK version");
  const pythonVersion = readPythonVersion(join(pythonSource, "pyproject.toml"));
  const pythonMinimumControlPlaneVersion = readPythonMinimumControlPlaneVersion(
    join(pythonSource, "pyproject.toml"),
  );
  assertMatchingBetaVersions(npmVersion, pythonVersion);
  const npmMinimumControlPlaneVersion = requireString(
    (npmManifest.polaris as JSONRecord | undefined)?.minimumControlPlaneVersion,
    "npm minimum Control Plane version",
  );
  if (npmMinimumControlPlaneVersion !== pythonMinimumControlPlaneVersion) {
    throw new Error(
      `Polaris SDK minimum Control Plane versions disagree: npm ${npmMinimumControlPlaneVersion}, Python ${pythonMinimumControlPlaneVersion}.`,
    );
  }
  if (npmManifest.name !== "@polaris-agents/sdk" || npmManifest.private !== true) {
    throw new Error("The source npm SDK must remain the private @polaris-agents/sdk workspace.");
  }
  if (npmManifest.repository === undefined) {
    throw new Error("The npm SDK must declare its exact GitHub repository for trusted publishing.");
  }

  const requiredDist = ["index.mjs", "index.cjs", "index.d.mts", "index.d.cts"];
  for (const file of requiredDist) requireRegularFile(join(npmSource, "dist", file));
  requireRegularFile(join(npmSource, "README.md"));
  requireRegularFile(join(pythonSource, "README.md"));
  requireRegularFile(join(pythonSource, "src", "polaris_agents", "_generated.py"));

  const npmDirectory = join(output, "npm");
  mkdirSync(npmDirectory, { recursive: true, mode: 0o755 });
  cpSync(join(npmSource, "dist"), join(npmDirectory, "dist"), { recursive: true });
  cpSync(join(npmSource, "README.md"), join(npmDirectory, "README.md"));
  const publicManifest = publicNPMManifest(npmManifest);
  writeFileSync(
    join(npmDirectory, "package.json"),
    `${JSON.stringify(publicManifest, null, 2)}\n`,
    {
      mode: 0o644,
    },
  );

  const sourceFiles = collectRegularFiles(npmDirectory).map((path) => ({
    path: path.slice(output.length + 1),
    sha256: sha256(readFileSync(path)),
  }));
  const preparation = {
    npmVersion,
    pythonVersion,
    minimumControlPlaneVersion: npmMinimumControlPlaneVersion,
    npmDirectory,
    sourceFiles,
  };
  writeFileSync(
    join(output, "release-manifest.json"),
    `${JSON.stringify(preparation, null, 2)}\n`,
    {
      mode: 0o644,
    },
  );
  return preparation;
}

export function publicNPMManifest(source: JSONRecord): JSONRecord {
  const allowed = [
    "name",
    "version",
    "description",
    "license",
    "repository",
    "homepage",
    "bugs",
    "type",
    "main",
    "module",
    "types",
    "exports",
    "files",
    "engines",
    "publishConfig",
    "polaris",
  ];
  const result: JSONRecord = {};
  for (const key of allowed) {
    if (source[key] !== undefined) result[key] = source[key];
  }
  result.private = false;
  return result;
}

export function assertMatchingBetaVersions(npmVersion: string, pythonVersion: string): void {
  const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-beta\.(0|[1-9]\d*)$/.exec(npmVersion);
  if (!match) {
    throw new Error("Polaris npm releases must use an explicit semver beta version.");
  }
  const expectedPython = `${match[1]}.${match[2]}.${match[3]}b${match[4]}`;
  if (pythonVersion !== expectedPython) {
    throw new Error(
      `Polaris SDK versions disagree: npm ${npmVersion} requires Python ${expectedPython}, found ${pythonVersion}.`,
    );
  }
}

function readPythonVersion(path: string): string {
  const source = readFileSync(path, "utf8");
  const project = /^\[project\]$/m.exec(source);
  if (!project) throw new Error("Python SDK pyproject.toml has no [project] table.");
  const afterProject = source.slice(project.index + project[0].length);
  const tableEnd = afterProject.search(/^\[/m);
  const projectBody = tableEnd < 0 ? afterProject : afterProject.slice(0, tableEnd);
  const matches = [...projectBody.matchAll(/^version\s*=\s*"([^"]+)"\s*$/gm)];
  if (matches.length !== 1) throw new Error("Python SDK must declare exactly one project version.");
  return matches[0]![1]!;
}

function readPythonMinimumControlPlaneVersion(path: string): string {
  const source = readFileSync(path, "utf8");
  const table = /^\[tool\.polaris\]$/m.exec(source);
  if (!table) throw new Error("Python SDK pyproject.toml has no [tool.polaris] table.");
  const afterTable = source.slice(table.index + table[0].length);
  const tableEnd = afterTable.search(/^\[/m);
  const body = tableEnd < 0 ? afterTable : afterTable.slice(0, tableEnd);
  const matches = [...body.matchAll(/^minimum-control-plane-version\s*=\s*"([^"]+)"\s*$/gm)];
  if (matches.length !== 1) {
    throw new Error("Python SDK must declare exactly one minimum Control Plane version.");
  }
  return matches[0]![1]!;
}

function readJSON(path: string): JSONRecord {
  const parsed: unknown = JSON.parse(readFileSync(path, "utf8"));
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`${basename(path)} must contain a JSON object.`);
  }
  return parsed as JSONRecord;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.trim() === "") throw new Error(`${label} is missing.`);
  return value;
}

function requireRegularFile(path: string): void {
  if (!existsSync(path) || !lstatSync(path).isFile() || lstatSync(path).isSymbolicLink()) {
    throw new Error(`Required Polaris release file is missing or unsafe: ${path}`);
  }
}

function collectRegularFiles(root: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`Polaris release staging refuses symlink: ${path}`);
    if (entry.isDirectory()) files.push(...collectRegularFiles(path));
    else if (entry.isFile()) files.push(path);
    else throw new Error(`Polaris release staging refuses special file: ${path}`);
  }
  return files.toSorted();
}

function sha256(value: Buffer): string {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}
