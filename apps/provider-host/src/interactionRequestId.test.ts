import { describe, expect, it } from "vitest";

import { providerInteractionRequestId } from "./interactionRequestId";

describe("Provider interaction request IDs", () => {
  it("keeps long Codex native IDs bounded, stable, and Generation-scoped", () => {
    const nativeId = `approval-${"界".repeat(200)}`;
    const generationSeven = providerInteractionRequestId("codex", 7, undefined, nativeId);
    const sameGeneration = providerInteractionRequestId("codex", 7, undefined, nativeId);
    const generationEight = providerInteractionRequestId("codex", 8, undefined, nativeId);

    expect(Buffer.byteLength(generationSeven)).toBeLessThanOrEqual(200);
    expect(Buffer.byteLength(generationEight)).toBeLessThanOrEqual(200);
    expect(generationSeven).toBe(sameGeneration);
    expect(generationSeven).toMatch(/^codex:generation-7:/);
    expect(generationEight).toMatch(/^codex:generation-8:/);
    expect(generationEight).not.toBe(generationSeven);
  });

  it("preserves a deterministic Claude collision suffix inside the byte limit", () => {
    const nativeId = `tool-${"x".repeat(300)}`;
    const original = providerInteractionRequestId("claude", 9, "approval", nativeId);
    const duplicate = providerInteractionRequestId("claude", 9, "approval", nativeId, 1);

    expect(Buffer.byteLength(original)).toBeLessThanOrEqual(200);
    expect(Buffer.byteLength(duplicate)).toBeLessThanOrEqual(200);
    expect(duplicate).toMatch(/^claude:generation-9:approval:/);
    expect(duplicate).toMatch(/:1$/);
    expect(duplicate).not.toBe(original);
  });
});
