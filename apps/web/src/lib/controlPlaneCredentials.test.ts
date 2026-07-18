import { describe, expect, it } from "vitest";

import type { ControlPlaneCredential } from "./controlPlaneClient";
import { listUsableControlPlaneCredentials } from "./controlPlaneCredentials";

function credential(
  input: Partial<ControlPlaneCredential> & Pick<ControlPlaneCredential, "id" | "purpose">,
): ControlPlaneCredential {
  const { id, purpose, ...overrides } = input;
  return {
    id,
    tenantId: "tenant-1",
    organizationId: null,
    scope: "tenant",
    scopeUserId: null,
    selectorOrganizationId: null,
    selectorModel: null,
    autoSelectEnabled: false,
    name: id,
    purpose,
    provider: purpose === "git" ? "git" : "codex",
    credentialType: purpose === "git" ? "https_token" : "api_key",
    kmsProvider: "local",
    kmsKeyId: "test",
    version: 1,
    createdBy: "user-1",
    updatedBy: "user-1",
    expiresAt: null,
    revokedAt: null,
    createdAt: "2026-07-13T00:00:00Z",
    updatedAt: "2026-07-13T00:00:00Z",
    ...overrides,
  };
}

describe("listUsableControlPlaneCredentials", () => {
  it("keeps purpose, provider, availability, and Credential scope isolated", () => {
    const items = [
      credential({ id: "provider-codex", purpose: "provider" }),
      credential({ id: "provider-claude", purpose: "provider", provider: "claudeAgent" }),
      credential({ id: "git-tenant", purpose: "git" }),
      credential({
        id: "git-organization",
        purpose: "git",
        scope: "organization",
        organizationId: "organization-1",
      }),
      credential({
        id: "git-other-organization",
        purpose: "git",
        scope: "organization",
        organizationId: "organization-2",
      }),
      credential({
        id: "provider-user",
        purpose: "provider",
        scope: "user",
        scopeUserId: "user-1",
      }),
      credential({
        id: "provider-other-user",
        purpose: "provider",
        scope: "user",
        scopeUserId: "user-2",
      }),
      credential({
        id: "provider-model",
        purpose: "provider",
        selectorModel: "gpt-5.6",
      }),
      credential({
        id: "git-expired",
        purpose: "git",
        expiresAt: "2026-07-12T23:59:59Z",
      }),
      credential({
        id: "git-revoked",
        purpose: "git",
        revokedAt: "2026-07-12T23:59:59Z",
      }),
    ];

    expect(
      listUsableControlPlaneCredentials(items, {
        purpose: "provider",
        provider: "codex",
        organizationId: "organization-1",
        userId: "user-1",
        model: "gpt-5.6",
        now: Date.parse("2026-07-13T00:00:00Z"),
      }).map((item) => item.id),
    ).toEqual(["provider-codex", "provider-user", "provider-model"]);
    expect(
      listUsableControlPlaneCredentials(items, {
        purpose: "git",
        organizationId: "organization-1",
        now: Date.parse("2026-07-13T00:00:00Z"),
      }).map((item) => item.id),
    ).toEqual(["git-tenant", "git-organization"]);
  });

  it("matches canonical provider requests against lowercased stored provider codes", () => {
    const items = [
      credential({ id: "provider-claudeagent", purpose: "provider", provider: "claudeagent" }),
      credential({ id: "provider-claude", purpose: "provider", provider: "claude" }),
      credential({ id: "provider-codex", purpose: "provider", provider: "codex" }),
    ];

    expect(
      listUsableControlPlaneCredentials(items, {
        purpose: "provider",
        provider: "claudeAgent",
        organizationId: null,
        now: Date.parse("2026-07-13T00:00:00Z"),
      }).map((item) => item.id),
    ).toEqual(["provider-claudeagent"]);
  });
});
