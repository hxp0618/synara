import { parse, stringify } from "yaml";

const operationMethods = new Set([
  "get",
  "put",
  "post",
  "delete",
  "options",
  "head",
  "patch",
  "trace",
]);

type UnknownRecord = Record<string, unknown>;

export function buildStage7PublicOpenAPI(source: string): {
  document: string;
  operationIds: string[];
} {
  const parsed = parse(source) as unknown;
  if (!isRecord(parsed) || !isRecord(parsed.paths) || !isRecord(parsed.info)) {
    throw new Error("Stage 7 OpenAPI source must contain info and paths objects.");
  }

  const paths: UnknownRecord = {};
  const operationIds: string[] = [];
  for (const [path, rawPathItem] of Object.entries(parsed.paths)) {
    if (!isRecord(rawPathItem)) continue;
    const publicPathItem: UnknownRecord = {};
    for (const [key, value] of Object.entries(rawPathItem)) {
      if (!operationMethods.has(key)) {
        publicPathItem[key] = value;
        continue;
      }
      if (!isRecord(value) || value["x-synara-contract-status"] !== "codegen-ready") continue;
      if (typeof value.operationId !== "string" || value.operationId.length === 0) {
        throw new Error(`${key.toUpperCase()} ${path} is codegen-ready without an operationId.`);
      }
      publicPathItem[key] = value;
      operationIds.push(value.operationId);
    }
    if (Object.keys(publicPathItem).some((key) => operationMethods.has(key)))
      paths[path] = publicPathItem;
  }
  if (operationIds.length === 0)
    throw new Error("Stage 7 public OpenAPI has no codegen-ready operations.");

  parsed.paths = paths;
  parsed.info = {
    ...parsed.info,
    description:
      "Codegen-ready public-beta contract for the Polaris Developer API. Operations omitted from this reference are not yet available as external compatibility commitments.",
  };
  delete parsed["x-synara-route-surfaces"];

  return {
    document: stringify(parsed, { lineWidth: 100, aliasDuplicateObjects: false }),
    operationIds: operationIds.sort(),
  };
}

function isRecord(value: unknown): value is UnknownRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
