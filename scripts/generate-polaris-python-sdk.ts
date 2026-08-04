import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";

import { parse } from "yaml";

import { buildStage7PublicOpenAPI } from "./lib/stage7-public-openapi";

type RecordValue = Record<string, any>;

const check = process.argv.includes("--check");
const positional = process.argv.slice(2).filter((value) => value !== "--check");
const sourcePath = resolve(positional[0] ?? "docs/api/openapi.yaml");
const outputPath = resolve(
  positional[1] ?? "packages/polaris-python/src/polaris_agents/_generated.py",
);
const publicDocument = parse(
  buildStage7PublicOpenAPI(await readFile(sourcePath, "utf8")).document,
) as RecordValue;
const schemas = publicDocument.components.schemas as RecordValue;
const selected = new Set([
  "Artifact",
  "ArtifactPage",
  "ArtifactDownloadGrant",
  "ArtifactUploadGrant",
  "CreateArtifactRequest",
  "CompleteArtifactRequest",
  "CreateSessionRequest",
  "Session",
  "SessionPage",
  "CreateTurnRequest",
  "Turn",
  "SessionEvent",
  "SessionEventPage",
  "ResolveApprovalRequest",
  "ResolveUserInputRequest",
  "Interaction",
  "InteractionPage",
  "PendingInteraction",
  "PendingInteractionPage",
  "CreateProjectRequest",
  "UpdateProjectRequest",
  "ProviderCapabilityProjection",
  "ProviderCapabilityItem",
  "SwitchSessionModelRequest",
  "SessionUsage",
  "ExecutionUsage",
  "PlatformCharge",
  "DeveloperExecution",
  "DeveloperControlCommand",
  "SteerActiveTurnRequest",
  "CompactSessionRequest",
  "ReviewTarget",
  "StartSessionReviewRequest",
  "DeveloperQueuedSessionOperation",
  "RollbackSessionRequest",
  "RollbackSessionResult",
  "ForkSessionRequest",
  "ForkSessionResult",
  "Project",
  "ProjectPage",
  "CreateExecutionTargetRequest",
  "ExecutionTarget",
  "CreateExecutionTargetProvisioningOperationRequest",
  "ExecutionTargetProvisioningOperation",
  "ErrorEnvelope",
]);

for (const name of [...selected]) collectReferences(schemas[name]);

const operations: Array<[string, string, string]> = [];
for (const [path, pathItem] of Object.entries(publicDocument.paths as RecordValue)) {
  for (const [method, operation] of Object.entries(pathItem as RecordValue)) {
    if (!isRecord(operation) || typeof operation.operationId !== "string") continue;
    operations.push([operation.operationId, method.toUpperCase(), path]);
  }
}
operations.sort(([left], [right]) => left.localeCompare(right));

const lines = [
  "# Generated from docs/api/openapi.yaml. Do not edit by hand.",
  "from __future__ import annotations",
  "",
  "from typing import Any, Final, Literal, Required, TypeAlias, TypedDict",
  "",
  ...[...selected].map((name) => renderSchema(name, schemas[name])),
  "OPERATIONS: Final[dict[str, tuple[str, str]]] = {",
  ...operations.map(
    ([id, method, path]) =>
      `    ${JSON.stringify(id)}: (${JSON.stringify(method)}, ${JSON.stringify(path)}),`,
  ),
  "}",
];
const output = `${lines.join("\n")}\n`;
if (check) {
  if ((await readFile(outputPath, "utf8")) !== output) {
    throw new Error("Python SDK generated types are stale. Run the generator without --check.");
  }
  console.log(`Python SDK generated types match ${operations.length} public operations.`);
} else {
  await writeFile(outputPath, output);
  console.log(`Generated Python SDK types and ${operations.length} public operations.`);
}

function collectReferences(value: unknown): void {
  if (Array.isArray(value)) {
    for (const item of value) collectReferences(item);
    return;
  }
  if (!isRecord(value)) return;
  if (typeof value.$ref === "string") {
    const name = value.$ref.split("/").at(-1)!;
    if (!selected.has(name)) {
      selected.add(name);
      collectReferences(schemas[name]);
    }
  }
  for (const child of Object.values(value)) collectReferences(child);
}

function renderSchema(name: string, schema: RecordValue): string {
  if (schema.type !== "object") return `${name}: TypeAlias = ${annotation(schema)}\n`;
  const required = new Set<string>(schema.required ?? []);
  const properties = Object.entries((schema.properties ?? {}) as RecordValue);
  const body = properties.length
    ? properties.map(([property, value]) => {
        const type = annotation(value as RecordValue);
        return `    ${property}: ${required.has(property) ? `Required[${type}]` : type}`;
      })
    : ["    pass"];
  return `class ${name}(TypedDict, total=False):\n${body.join("\n")}\n`;
}

function annotation(schema: RecordValue): string {
  if (typeof schema.$ref === "string") return schema.$ref.split("/").at(-1)!;
  if (Array.isArray(schema.oneOf)) return union(schema.oneOf.map(annotation));
  if (Array.isArray(schema.enum)) return `Literal[${schema.enum.map(pyLiteral).join(", ")}]`;
  if (Array.isArray(schema.type)) return union(schema.type.map((type: string) => primitive(type)));
  if (schema.type === "array") return `list[${annotation(schema.items ?? {})}]`;
  if (schema.type === "object") return "dict[str, Any]";
  return primitive(schema.type);
}

function union(values: string[]): string {
  return [...new Set(values)].join(" | ");
}

function primitive(type: string | undefined): string {
  if (type === "string") return "str";
  if (type === "integer") return "int";
  if (type === "number") return "float";
  if (type === "boolean") return "bool";
  if (type === "null") return "None";
  return "Any";
}

function pyLiteral(value: unknown): string {
  if (value === null) return "None";
  if (value === true) return "True";
  if (value === false) return "False";
  return JSON.stringify(value);
}

function isRecord(value: unknown): value is RecordValue {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
