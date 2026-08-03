import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { ReleaseNoticeSection } from "./ReleaseNoticeSection";

describe("ReleaseNoticeSection", () => {
  it("renders a breaking-change deadline, action, and safe external links", () => {
    const markup = renderToStaticMarkup(
      <ReleaseNoticeSection
        notice={{
          id: "worker-protocol-v2",
          classification: "breaking-change",
          title: "Worker protocol v1 retirement",
          summary: "New work will require protocol v2.",
          affectedAudience: "Self-managed enterprise administrators",
          requiredAction: "Upgrade every worker before the deadline.",
          firstPublishedAt: "2026-07-01T00:00:00Z",
          effectiveAt: "2026-09-29T00:00:00Z",
          migrationGuideUrl: "https://docs.example.com/migrate",
          supportUrl: "https://support.example.com",
        }}
      />,
    );

    expect(markup).toContain("Breaking change");
    expect(markup).toContain("Upgrade every worker before the deadline.");
    expect(markup).toContain('dateTime="2026-09-29T00:00:00Z"');
    expect(markup).toContain('target="_blank" rel="noopener noreferrer"');
  });
});
