export class PolarisError extends Error {
  override readonly name = "PolarisError";

  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId: string | null,
    readonly details: Readonly<Record<string, unknown>> | null,
    readonly retryAfterSeconds: number | null,
  ) {
    super(message);
  }
}

export class PolarisTransportError extends Error {
  override readonly name = "PolarisTransportError";

  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
  }
}

export class PolarisSequenceGapError extends Error {
  override readonly name = "PolarisSequenceGapError";

  constructor(
    readonly expectedSequence: number,
    readonly receivedSequence: number,
  ) {
    super(
      `Session Event sequence gap: expected ${expectedSequence}, received ${receivedSequence}.`,
    );
  }
}
