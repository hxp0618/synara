import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { EnterpriseUiHostProvider } from "./EnterpriseUiHost";
import { CredentialPayloadFields } from "./CredentialPayloadFields";
import { createCredentialPayloadDraft } from "./credentialPayloadForm";
import { TEST_ENTERPRISE_UI_HOST } from "./testHost";

describe("CredentialPayloadFields", () => {
  it("renders managed provider API key fields without the advanced JSON editor", () => {
    const markup = renderToStaticMarkup(
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <CredentialPayloadFields
          draft={createCredentialPayloadDraft("provider_codex")}
          kind="provider_codex"
          mode="create"
          onChange={() => undefined}
        />
      </EnterpriseUiHostProvider>,
    );

    expect(markup).toContain("Codex third-party API key");
    expect(markup).toContain("Base URL (optional)");
    expect(markup).not.toContain("Advanced provider code");
    expect(markup).not.toContain("secret JSON payload");
  });

  it("keeps the advanced provider JSON fields for non-managed runtimes", () => {
    const markup = renderToStaticMarkup(
      <EnterpriseUiHostProvider components={TEST_ENTERPRISE_UI_HOST}>
        <CredentialPayloadFields
          draft={createCredentialPayloadDraft("provider_advanced")}
          kind="provider_advanced"
          mode="rotate"
          onChange={() => undefined}
        />
      </EnterpriseUiHostProvider>,
    );

    expect(markup).toContain("Advanced provider code");
    expect(markup).toContain("Advanced credential type");
    expect(markup).toContain("Replacement secret JSON payload");
  });
});
