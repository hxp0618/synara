import { chmodSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import { validateMacDistributionEnvironment } from "./mac-distribution-preflight.ts";

const temporaryRoots: string[] = [];
const PRIVATE_KEY = `-----BEGIN PRIVATE KEY-----
${"A".repeat(96)}
-----END PRIVATE KEY-----
`;

afterEach(() => {
  for (const root of temporaryRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

function fixture(overrides: Record<string, string | undefined> = {}) {
  const root = mkdtempSync(join(tmpdir(), "synara-mac-distribution-preflight-"));
  temporaryRoots.push(root);
  const privateKeyPath = join(root, "AuthKey_ABCDEFGHIJ.p8");
  writeFileSync(privateKeyPath, PRIVATE_KEY, { mode: 0o600 });
  chmodSync(privateKeyPath, 0o600);
  return {
    privateKeyPath,
    environment: {
      CSC_LINK: "developer-id-certificate-source",
      CSC_KEY_PASSWORD: "not-printed",
      APPLE_API_KEY: privateKeyPath,
      APPLE_API_KEY_ID: "ABCDEFGHIJ",
      APPLE_API_ISSUER: "12345678-1234-4234-9234-1234567890ab",
      APPLE_TEAM_ID: "TEAM123456",
      ...overrides,
    },
  };
}

describe("macOS distribution preflight", () => {
  it("returns only non-secret structural evidence for a private credential set", () => {
    const { environment } = fixture();
    const evidence = validateMacDistributionEnvironment({ environment, hostPlatform: "darwin" });
    expect(evidence).toEqual({
      status: "ready-for-signed-build",
      developerIdCredentialSourcePresent: true,
      notaryPrivateKey: {
        regularFile: true,
        ownedByCurrentUser: true,
        privateMode: true,
        pkcs8Pem: true,
      },
      appleApiKeyIdFormatValid: true,
      appleApiIssuerFormatValid: true,
      appleTeamIdFormatValid: true,
    });
    const serializedEvidence = JSON.stringify(evidence);
    expect(serializedEvidence).not.toContain(environment.CSC_LINK);
    expect(serializedEvidence).not.toContain(environment.CSC_KEY_PASSWORD);
    expect(serializedEvidence).not.toContain(environment.APPLE_API_KEY);
  });

  it.each([
    "CSC_LINK",
    "CSC_KEY_PASSWORD",
    "APPLE_API_KEY",
    "APPLE_API_KEY_ID",
    "APPLE_API_ISSUER",
    "APPLE_TEAM_ID",
  ])("rejects a missing %s without inspecting later build inputs", (name) => {
    const { environment } = fixture({ [name]: "" });
    expect(() =>
      validateMacDistributionEnvironment({ environment, hostPlatform: "darwin" }),
    ).toThrow(`requires ${name}`);
  });

  it("rejects a group-readable notary private key", () => {
    const { privateKeyPath, environment } = fixture();
    chmodSync(privateKeyPath, 0o640);
    expect(() =>
      validateMacDistributionEnvironment({ environment, hostPlatform: "darwin" }),
    ).toThrow("must not grant group or other permissions");
  });

  it("rejects a symlinked notary private key", () => {
    const { privateKeyPath, environment } = fixture();
    const linkPath = `${privateKeyPath}.link`;
    symlinkSync(privateKeyPath, linkPath);
    expect(() =>
      validateMacDistributionEnvironment({
        environment: { ...environment, APPLE_API_KEY: linkPath },
        hostPlatform: "darwin",
      }),
    ).toThrow("regular non-symlink file");
  });

  it("rejects malformed Apple identifiers, issuer UUIDs and private-key content", () => {
    const invalidKeyId = fixture({ APPLE_API_KEY_ID: "lowercase" });
    expect(() =>
      validateMacDistributionEnvironment({
        environment: invalidKeyId.environment,
        hostPlatform: "darwin",
      }),
    ).toThrow("APPLE_API_KEY_ID");

    const invalidIssuer = fixture({ APPLE_API_ISSUER: "not-a-uuid" });
    expect(() =>
      validateMacDistributionEnvironment({
        environment: invalidIssuer.environment,
        hostPlatform: "darwin",
      }),
    ).toThrow("APPLE_API_ISSUER must be a UUID");

    const invalidPrivateKey = fixture();
    writeFileSync(invalidPrivateKey.privateKeyPath, "not-a-private-key".repeat(8), { mode: 0o600 });
    expect(() =>
      validateMacDistributionEnvironment({
        environment: invalidPrivateKey.environment,
        hostPlatform: "darwin",
      }),
    ).toThrow("PKCS#8 PEM");
  });

  it("rejects execution away from a native macOS build host", () => {
    const { environment } = fixture();
    expect(() =>
      validateMacDistributionEnvironment({ environment, hostPlatform: "linux" }),
    ).toThrow("must run on macOS");
  });
});
