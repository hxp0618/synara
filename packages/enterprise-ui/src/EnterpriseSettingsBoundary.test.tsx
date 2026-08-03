import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import { EnterpriseSettingsBoundary } from "./EnterpriseSettingsBoundary";

describe("EnterpriseSettingsBoundary", () => {
  it("passes the registered destination to the host renderer", () => {
    const renderer = vi.fn(({ destination }: { destination: string }) => (
      <p>destination:{destination}</p>
    ));

    const markup = renderToStaticMarkup(
      <EnterpriseSettingsBoundary
        destination="organization-identity"
        renderer={renderer}
        loadingFallback={<p>Loading Tenant settings</p>}
      />,
    );

    expect(markup).toContain("destination:organization-identity");
    expect(renderer).toHaveBeenCalledWith({ destination: "organization-identity" }, undefined);
  });
});
