import { describe, expect, it } from "vitest";
import { resolveReleaseSourceTagMode } from "./release-source-tag-policy.ts";

describe("release source tag policy", () => {
  it("uses an existing exact tag for ordinary publication", () => {
    expect(
      resolveReleaseSourceTagMode({
        tag: "v1.2.3",
        publishRelease: true,
        enterpriseGaCandidate: false,
        refType: "tag",
        refName: "v1.2.3",
      }),
    ).toBe("existing-tag");
  });

  it("allows a protected Enterprise GA run to reserve a not-yet-created tag from a branch", () => {
    expect(
      resolveReleaseSourceTagMode({
        tag: "v1.2.3",
        publishRelease: true,
        enterpriseGaCandidate: true,
        refType: "branch",
        refName: "codex/stage6-rc",
      }),
    ).toBe("planned-tag");
  });

  it("keeps build-only branch runs tagless", () => {
    expect(
      resolveReleaseSourceTagMode({
        tag: "v1.2.3",
        publishRelease: false,
        enterpriseGaCandidate: false,
        refType: "branch",
        refName: "main",
      }),
    ).toBe("none");
  });

  it("rejects ordinary branch publication and an unbound Enterprise run", () => {
    expect(() =>
      resolveReleaseSourceTagMode({
        tag: "v1.2.3",
        publishRelease: true,
        enterpriseGaCandidate: false,
        refType: "branch",
        refName: "main",
      }),
    ).toThrow("exact release tag");
    expect(() =>
      resolveReleaseSourceTagMode({
        tag: "v1.2.3",
        publishRelease: true,
        enterpriseGaCandidate: true,
        refType: "tag",
        refName: "v1.2.2",
      }),
    ).toThrow("must start from a branch");
  });
});
