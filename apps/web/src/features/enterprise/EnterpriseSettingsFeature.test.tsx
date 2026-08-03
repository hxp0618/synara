import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseSettingsFeature } from "./EnterpriseSettingsFeature";

describe("EnterpriseSettingsFeature", () => {
  it("keeps the Web loading presentation behind the shared package seam", () => {
    const markup = renderToStaticMarkup(
      <EnterpriseSettingsFeature destination="organization-overview" />,
    );

    expect(markup).toContain('role="status"');
    expect(markup).toContain("Loading the authenticated Tenant administration surface.");
  });
});
