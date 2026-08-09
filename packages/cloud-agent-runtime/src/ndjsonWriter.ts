import { Buffer } from "node:buffer";
import type { Writable } from "node:stream";

export const DEFAULT_NDJSON_MAX_PENDING_WRITES = 64;

export type BoundedNdjsonWriter = {
  readonly enqueue: (value: unknown) => void;
  readonly flush: () => Promise<void>;
};

export function createBoundedNdjsonWriter(options: {
  readonly target: Writable;
  readonly maximumMessageBytes: number;
  readonly maximumPendingWrites?: number;
}): BoundedNdjsonWriter {
  const maximumPendingWrites = options.maximumPendingWrites ?? DEFAULT_NDJSON_MAX_PENDING_WRITES;
  if (!Number.isSafeInteger(maximumPendingWrites) || maximumPendingWrites < 1) {
    throw new Error("NDJSON maximumPendingWrites must be a positive safe integer.");
  }

  let pendingWrites = 0;
  let failure: Error | undefined;
  let tail = Promise.resolve();

  const fail = (error: unknown): Error => {
    failure ??= error instanceof Error ? error : new Error(String(error));
    return failure;
  };

  return {
    enqueue(value) {
      if (failure) throw failure;

      const frame = Buffer.from(`${JSON.stringify(value)}\n`, "utf8");
      if (frame.byteLength > options.maximumMessageBytes) {
        throw fail(
          new Error(
            `Provider Host message is ${frame.byteLength} bytes, exceeding the negotiated ${options.maximumMessageBytes}-byte limit.`,
          ),
        );
      }
      if (pendingWrites >= maximumPendingWrites) {
        throw fail(
          new Error(
            `Provider Host stdout queue exceeded ${maximumPendingWrites} pending messages.`,
          ),
        );
      }

      pendingWrites += 1;
      // The promise chain is the sole stdout writer, preserving frame order and
      // preventing writes after a stream or backpressure failure.
      tail = tail
        .then(async () => {
          if (!failure) await writeWithBackpressure(options.target, frame);
        })
        .catch((error) => {
          fail(error);
        })
        .finally(() => {
          pendingWrites -= 1;
        });
    },
    async flush() {
      await tail;
      if (failure) throw failure;
    },
  };
}

async function writeWithBackpressure(target: Writable, frame: Buffer): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    let callbackComplete = false;
    let drainComplete = true;
    let settled = false;

    const cleanup = () => {
      target.off("error", onError);
      target.off("close", onClose);
      target.off("drain", onDrain);
    };
    const settle = (error?: Error) => {
      if (settled || (!error && (!callbackComplete || !drainComplete))) return;
      settled = true;
      cleanup();
      if (error) reject(error);
      else resolve();
    };
    const onError = (error: Error) => settle(error);
    const onClose = () =>
      settle(new Error("Provider Host stdout closed before the frame flushed."));
    const onDrain = () => {
      drainComplete = true;
      settle();
    };

    target.once("error", onError);
    target.once("close", onClose);
    const accepted = target.write(frame, (error) => {
      if (error) {
        settle(error);
        return;
      }
      callbackComplete = true;
      settle();
    });
    if (!accepted) {
      drainComplete = false;
      target.once("drain", onDrain);
    }
  });
}
