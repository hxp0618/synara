import { describe, expect, it } from "vitest";

import { classifyProviderResumeFailure } from "./providerResumeFallback";

describe("classifyProviderResumeFailure", () => {
  it("classifies the Codex missing-rollout resume failure as session_resume_invalid", () => {
    expect(
      classifyProviderResumeFailure(
        new Error(
          "thread/resume failed: no rollout found for thread id 01890a5d-ac96-774b-bcce-b302099ed01d",
        ),
      ),
    ).toBe("session_resume_invalid");
  });

  it("classifies missing and unknown rollout shapes as session_resume_invalid", () => {
    expect(classifyProviderResumeFailure(new Error("missing rollout for thread"))).toBe(
      "session_resume_invalid",
    );
    expect(classifyProviderResumeFailure(new Error("rollout thread-1 not found"))).toBe(
      "session_resume_invalid",
    );
  });

  it("keeps missing-thread and expired-session classification", () => {
    expect(classifyProviderResumeFailure(new Error("missing thread: thread-1"))).toBe(
      "session_resume_invalid",
    );
    expect(classifyProviderResumeFailure(new Error("session thread-1 has expired"))).toBe(
      "session_resume_expired",
    );
  });

  it("never reclassifies authentication, rate-limit, or transport failures", () => {
    expect(
      classifyProviderResumeFailure(new Error("Unauthorized: invalid API key")),
    ).toBeUndefined();
    expect(classifyProviderResumeFailure(new Error("too many requests"))).toBeUndefined();
    expect(
      classifyProviderResumeFailure(new Error("transport error: no rollout found")),
    ).toBeUndefined();
  });
});
