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
  it("keeps purpose, provider, availability, and organization scope isolated", () => {
    const items = [
      credential({ id: "provider-codex", purpose: "provider" }),
      credential({ id: "provider-claude", purpose: "provider", provider: "claudeAgent" }),
      credential({ id: "git-tenant", purpose: "git" }),
      credential({ id: "git-organization", purpose: "git", organizationId: "organization-1" }),
      credential({
        id: "git-other-organization",
        purpose: "git",
        organizationId: "organization-2",
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
        now: Date.parse("2026-07-13T00:00:00Z"),
      }).map((item) => item.id),
    ).toEqual(["provider-codex"]);
    expect(
      listUsableControlPlaneCredentials(items, {
        purpose: "git",
        organizationId: "organization-1",
        now: Date.parse("2026-07-13T00:00:00Z"),
      }).map((item) => item.id),
    ).toEqual(["git-tenant", "git-organization"]);
  });
});
