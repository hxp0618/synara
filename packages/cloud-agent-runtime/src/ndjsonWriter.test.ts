import { Writable } from "node:stream";
import { describe, expect, it } from "vitest";

import { createBoundedNdjsonWriter } from "./ndjsonWriter";

describe("bounded NDJSON writer", () => {
  it("enforces the encoded UTF-8 frame limit including its newline", async () => {
    const target = new RecordingWritable();
    const writer = createBoundedNdjsonWriter({ target, maximumMessageBytes: 15 });

    expect(() => writer.enqueue({ text: "😀" })).toThrow(/16 bytes/);
    await expect(writer.flush()).rejects.toThrow(/16 bytes/);
    expect(target.frames).toEqual([]);
  });

  it("serializes backpressured writes and flushes before resolving", async () => {
    const target = new RecordingWritable(10);
    const writer = createBoundedNdjsonWriter({ target, maximumMessageBytes: 1_024 });

    writer.enqueue({ sequence: 1 });
    writer.enqueue({ sequence: 2 });
    expect(target.frames).toEqual([]);

    await writer.flush();

    expect(target.maximumConcurrentWrites).toBe(1);
    expect(target.frames.map((frame) => JSON.parse(frame))).toEqual([
      { sequence: 1 },
      { sequence: 2 },
    ]);
  });

  it("fails closed when the pending queue reaches its configured bound", async () => {
    const target = new RecordingWritable(10);
    const writer = createBoundedNdjsonWriter({
      target,
      maximumMessageBytes: 1_024,
      maximumPendingWrites: 1,
    });

    writer.enqueue({ sequence: 1 });
    expect(() => writer.enqueue({ sequence: 2 })).toThrow(/queue exceeded 1/);
    await expect(writer.flush()).rejects.toThrow(/queue exceeded 1/);
  });
});

class RecordingWritable extends Writable {
  readonly frames: string[] = [];
  maximumConcurrentWrites = 0;
  private concurrentWrites = 0;

  constructor(private readonly delayMs = 0) {
    super({ highWaterMark: 1 });
  }

  override _write(
    chunk: Buffer,
    _encoding: BufferEncoding,
    callback: (error?: Error | null) => void,
  ): void {
    this.concurrentWrites += 1;
    this.maximumConcurrentWrites = Math.max(this.maximumConcurrentWrites, this.concurrentWrites);
    setTimeout(() => {
      this.frames.push(chunk.toString("utf8").trimEnd());
      this.concurrentWrites -= 1;
      callback();
    }, this.delayMs);
  }
}
