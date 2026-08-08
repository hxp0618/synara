import { describe, expect, it } from "vitest";

import { pages, quickstartCode } from "./content";

describe("developer documentation content", () => {
  it("keeps navigation slugs and section anchors unique", () => {
    expect(new Set(pages.map((page) => page.slug)).size).toBe(pages.length);
    for (const page of pages) {
      expect(new Set(page.sections.map((section) => section.id)).size).toBe(page.sections.length);
    }
  });

  it("uses the public SDK package and required server-side configuration", () => {
    expect(quickstartCode).toContain('from "@polaris-agents/sdk"');
    expect(quickstartCode).toContain("POLARIS_API_KEY");
    expect(quickstartCode).toContain("POLARIS_BASE_URL");
    expect(quickstartCode).toContain("POLARIS_TENANT_ID");
    expect(quickstartCode).toContain("POLARIS_ORGANIZATION_ID");
    expect(quickstartCode).toContain("polaris.projects.create");
    expect(quickstartCode).toContain("session.events()");
  });
});
