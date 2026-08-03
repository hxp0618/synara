import { describe, expect, it } from "vitest";

import { WHATS_NEW_ENTRIES } from "./entries";
import { validateReleaseNotice } from "./logic";

describe("curated release entries", () => {
  it("has unique release, feature, and notice identifiers", () => {
    const versions = WHATS_NEW_ENTRIES.map((entry) => entry.version);
    const featureIDs = WHATS_NEW_ENTRIES.flatMap((entry) =>
      entry.features.map((feature) => `${entry.version}:${feature.id}`),
    );
    const noticeIDs = WHATS_NEW_ENTRIES.flatMap((entry) =>
      (entry.notices ?? []).map((notice) => notice.id),
    );

    expect(new Set(versions).size).toBe(versions.length);
    expect(new Set(featureIDs).size).toBe(featureIDs.length);
    expect(new Set(noticeIDs).size).toBe(noticeIDs.length);
  });

  it("keeps every structured customer notice inside its timing contract", () => {
    const errors = WHATS_NEW_ENTRIES.flatMap((entry) =>
      (entry.notices ?? []).flatMap((notice) =>
        validateReleaseNotice(notice).map((message) => `${entry.version}/${notice.id}: ${message}`),
      ),
    );

    expect(errors).toEqual([]);
  });
});
