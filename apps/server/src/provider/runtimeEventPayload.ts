import type {
  CanonicalItemType,
  ContentDeltaPayload,
  ItemLifecyclePayload,
  RuntimeContentStreamKind,
} from "@synara/contracts";

type RuntimeItemStatus = "inProgress" | "completed" | "failed" | "declined";
type NonCommandContentStreamKind = Exclude<RuntimeContentStreamKind, "command_output">;

type ContentDeltaOptionalFields = {
  readonly contentIndex?: number;
  readonly summaryIndex?: number;
  readonly truncated?: boolean;
};

export type RuntimeCommandOutputDeltaPayload = ContentDeltaOptionalFields & {
  readonly streamKind: "command_output";
  readonly delta: string;
  readonly terminalId: string;
  readonly encoding: "utf-8";
  readonly byteOffset: number;
  readonly byteLength: number;
};

type RuntimeTextDeltaPayload = ContentDeltaOptionalFields & {
  readonly streamKind: NonCommandContentStreamKind;
  readonly delta: string;
};

export function makeRuntimeItemLifecyclePayload(input: {
  readonly itemType: CanonicalItemType;
  readonly status?: RuntimeItemStatus;
  readonly title?: string;
  readonly detail?: string;
  readonly data?: unknown;
}): ItemLifecyclePayload {
  const common = {
    ...(input.status ? { status: input.status } : {}),
    ...(input.title ? { title: input.title } : {}),
    ...(input.detail ? { detail: input.detail } : {}),
  };

  if (input.itemType === "command_execution") {
    const data = commandExecutionData(input.data);
    return {
      itemType: "command_execution",
      ...common,
      ...(data === undefined ? {} : { data }),
    };
  }

  const data = withoutTerminalData(input.data);
  return {
    itemType: input.itemType,
    ...common,
    ...(data === undefined ? {} : { data }),
  };
}

export function makeUtf8RuntimeContentDeltaPayload(input: {
  readonly streamKind: "command_output";
  readonly delta: string;
  readonly terminalId: string;
  readonly byteOffset: number;
  readonly contentIndex?: number;
  readonly summaryIndex?: number;
  readonly truncated?: boolean;
}): RuntimeCommandOutputDeltaPayload;
export function makeUtf8RuntimeContentDeltaPayload(input: {
  readonly streamKind: NonCommandContentStreamKind;
  readonly delta: string;
  readonly contentIndex?: number;
  readonly summaryIndex?: number;
  readonly truncated?: boolean;
}): RuntimeTextDeltaPayload;
export function makeUtf8RuntimeContentDeltaPayload(
  input:
    | {
        readonly streamKind: "command_output";
        readonly delta: string;
        readonly terminalId: string;
        readonly byteOffset: number;
        readonly contentIndex?: number;
        readonly summaryIndex?: number;
        readonly truncated?: boolean;
      }
    | {
        readonly streamKind: NonCommandContentStreamKind;
        readonly delta: string;
        readonly contentIndex?: number;
        readonly summaryIndex?: number;
        readonly truncated?: boolean;
      },
): ContentDeltaPayload {
  const optionalFields = {
    ...(input.contentIndex === undefined ? {} : { contentIndex: input.contentIndex }),
    ...(input.summaryIndex === undefined ? {} : { summaryIndex: input.summaryIndex }),
    ...(input.truncated === undefined ? {} : { truncated: input.truncated }),
  };

  if (input.streamKind === "command_output") {
    return {
      streamKind: "command_output",
      delta: input.delta,
      terminalId: input.terminalId,
      encoding: "utf-8",
      byteOffset: input.byteOffset,
      byteLength: Buffer.byteLength(input.delta, "utf8"),
      ...optionalFields,
    };
  }

  return {
    streamKind: input.streamKind,
    delta: input.delta,
    ...optionalFields,
  };
}

function commandExecutionData(
  value: unknown,
): Extract<ItemLifecyclePayload, { readonly itemType: "command_execution" }>["data"] {
  if (
    value === undefined ||
    value === null ||
    typeof value === "string" ||
    typeof value === "number" ||
    typeof value === "boolean" ||
    Array.isArray(value)
  ) {
    return value;
  }
  if (typeof value === "object") {
    return Object.fromEntries(Object.entries(value));
  }
  return undefined;
}

function withoutTerminalData(value: unknown): unknown {
  if (
    typeof value !== "object" ||
    value === null ||
    Array.isArray(value) ||
    !Object.hasOwn(value, "terminal")
  ) {
    return value;
  }
  return Object.fromEntries(Object.entries(value).filter(([key]) => key !== "terminal"));
}
