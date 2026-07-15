import { describe, expect, it } from "vitest";

import {
  buildCredentialPayload,
  createCredentialPayloadDraft,
  credentialFormKindForCredential,
  describeCredentialForm,
} from "./credentialPayloadForm";

describe("credentialPayloadForm", () => {
  it("maps every structured kind to its fixed purpose, provider, and type", () => {
    const draft = createCredentialPayloadDraft("provider");

    expect(describeCredentialForm("git_https", draft)).toEqual({
      purpose: "git",
      provider: "git",
      credentialType: "https_token",
    });
    expect(describeCredentialForm("git_ssh", draft).credentialType).toBe("ssh_key");
    expect(describeCredentialForm("registry_basic", draft)).toEqual({
      purpose: "registry",
      provider: "oci",
      credentialType: "basic",
    });
    expect(describeCredentialForm("registry_bearer", draft).credentialType).toBe("bearer_token");
    expect(describeCredentialForm("package_npm", draft)).toEqual({
      purpose: "package",
      provider: "npm",
      credentialType: "npm_token",
    });
    expect(describeCredentialForm("package_pypi", draft).provider).toBe("pypi");
  });

  it("normalizes provider metadata while preserving the opaque JSON payload", () => {
    const draft = {
      ...createCredentialPayloadDraft("provider"),
      provider: " OpenAI ",
      credentialType: " API_KEY ",
      jsonPayload: '{"apiKey":"provider-secret"}',
    };

    expect(describeCredentialForm("provider", draft)).toEqual({
      purpose: "provider",
      provider: "openai",
      credentialType: "api_key",
    });
    expect(buildCredentialPayload("provider", draft)).toEqual({
      apiKey: "provider-secret",
    });
  });

  it("builds the strict Git SSH payload including numeric port and optional passphrase", () => {
    const draft = {
      ...createCredentialPayloadDraft("git_ssh"),
      host: "github.com",
      port: "22",
      username: "git",
      privateKey: "private-key-material",
      privateKeyPassphrase: "passphrase-secret",
      hostKey: "ssh-ed25519 host-key-material",
    };

    expect(buildCredentialPayload("git_ssh", draft)).toEqual({
      host: "github.com",
      port: 22,
      username: "git",
      privateKey: "private-key-material",
      privateKeyPassphrase: "passphrase-secret",
      hostKey: "ssh-ed25519 host-key-material",
    });
    expect(() => buildCredentialPayload("git_ssh", { ...draft, port: "0" })).toThrow(
      "between 1 and 65535",
    );
  });

  it("builds Registry and Package payloads without accepting arbitrary fields", () => {
    const registryDraft = {
      ...createCredentialPayloadDraft("registry_basic"),
      host: "registry.example.com",
      username: "robot",
      secret: "registry-secret",
    };
    expect(buildCredentialPayload("registry_basic", registryDraft)).toEqual({
      host: "registry.example.com",
      username: "robot",
      password: "registry-secret",
    });

    const npmDraft = {
      ...createCredentialPayloadDraft("package_npm"),
      secret: "npm-package-secret",
      scopes: "@Company, @internal\n@company",
    };
    expect(buildCredentialPayload("package_npm", npmDraft)).toEqual({
      registryUrl: "https://registry.npmjs.org/",
      token: "npm-package-secret",
      scopes: ["@company", "@internal"],
    });
  });

  it("recognizes supported persisted credential combinations and rejects drift", () => {
    expect(
      credentialFormKindForCredential({
        purpose: "git",
        provider: "git",
        credentialType: "ssh_key",
      }),
    ).toBe("git_ssh");
    expect(
      credentialFormKindForCredential({
        purpose: "package",
        provider: "npm",
        credentialType: "npm_token",
      }),
    ).toBe("package_npm");
    expect(
      credentialFormKindForCredential({
        purpose: "registry",
        provider: "docker",
        credentialType: "basic",
      }),
    ).toBeNull();
  });
});
