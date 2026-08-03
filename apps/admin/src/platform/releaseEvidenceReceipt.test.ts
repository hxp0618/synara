import { describe, expect, it } from "vitest";

import { encodeCandidateEvidenceReceipt, readFinalReviewReceipt } from "./releaseEvidenceReceipt";

describe("candidate evidence receipt upload", () => {
  it("hashes and base64-encodes the exact selected bytes", async () => {
    const receipt = await encodeCandidateEvidenceReceipt(
      "candidate-receipt.json",
      new TextEncoder().encode("{}"),
    );

    expect(receipt).toEqual({
      name: "candidate-receipt.json",
      sha256: "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
      base64: "e30=",
    });
  });

  it("rejects empty and oversized files before upload", async () => {
    await expect(encodeCandidateEvidenceReceipt("empty.json", new Uint8Array())).rejects.toThrow(
      "between 1 byte and 32 KiB",
    );
    await expect(
      encodeCandidateEvidenceReceipt("large.json", new Uint8Array(32 * 1024 + 1)),
    ).rejects.toThrow("between 1 byte and 32 KiB");
  });

  it("accepts a bounded Final Review archive up to its independent 512 KiB limit", async () => {
    const bytes = new TextEncoder().encode('{"schemaVersion":"final-review"}');
    const receipt = await readFinalReviewReceipt({
      name: "final-review.json",
      arrayBuffer: async () => bytes.buffer,
    });

    expect(receipt.name).toBe("final-review.json");
    expect(receipt.base64).toBe("eyJzY2hlbWFWZXJzaW9uIjoiZmluYWwtcmV2aWV3In0=");
    await expect(
      readFinalReviewReceipt({
        name: "oversized-final-review.json",
        arrayBuffer: async () => new Uint8Array(512 * 1024 + 1).buffer,
      }),
    ).rejects.toThrow("between 1 byte and 512 KiB");
  });
});
