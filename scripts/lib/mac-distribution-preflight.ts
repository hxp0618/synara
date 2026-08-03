// FILE: mac-distribution-preflight.ts
// Purpose: Fails closed before a signed macOS build when distribution credential inputs are unsafe or incomplete.
// Layer: Release/build helper

import { lstatSync, readFileSync } from "node:fs";
import { isAbsolute } from "node:path";

const APPLE_IDENTIFIER_PATTERN = /^[A-Z0-9]{10}$/;
const APPLE_ISSUER_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const MAX_NOTARY_PRIVATE_KEY_BYTES = 64 * 1024;
const MIN_NOTARY_PRIVATE_KEY_BYTES = 64;

export interface MacDistributionEnvironmentInput {
  readonly environment: Readonly<Record<string, string | undefined>>;
  readonly hostPlatform?: NodeJS.Platform;
  readonly currentUid?: number;
}

export interface MacDistributionPreflightEvidence {
  readonly status: "ready-for-signed-build";
  readonly developerIdCredentialSourcePresent: true;
  readonly notaryPrivateKey: {
    readonly regularFile: true;
    readonly ownedByCurrentUser: true;
    readonly privateMode: true;
    readonly pkcs8Pem: true;
  };
  readonly appleApiKeyIdFormatValid: true;
  readonly appleApiIssuerFormatValid: true;
  readonly appleTeamIdFormatValid: true;
}

function requireSecret(environment: Readonly<Record<string, string | undefined>>, name: string) {
  const value = environment[name]?.trim();
  if (!value) {
    throw new Error(`Signed macOS distribution preflight requires ${name}.`);
  }
  return value;
}

function requireAppleIdentifier(value: string, name: string): void {
  if (!APPLE_IDENTIFIER_PATTERN.test(value)) {
    throw new Error(`${name} must be a 10-character uppercase Apple identifier.`);
  }
}

export function validateMacDistributionEnvironment(
  input: MacDistributionEnvironmentInput,
): MacDistributionPreflightEvidence {
  if ((input.hostPlatform ?? process.platform) !== "darwin") {
    throw new Error("Signed macOS distribution preflight must run on macOS.");
  }

  requireSecret(input.environment, "CSC_LINK");
  requireSecret(input.environment, "CSC_KEY_PASSWORD");
  const privateKeyPath = requireSecret(input.environment, "APPLE_API_KEY");
  const keyId = requireSecret(input.environment, "APPLE_API_KEY_ID");
  const issuer = requireSecret(input.environment, "APPLE_API_ISSUER");
  const teamId = requireSecret(input.environment, "APPLE_TEAM_ID");

  requireAppleIdentifier(keyId, "APPLE_API_KEY_ID");
  requireAppleIdentifier(teamId, "APPLE_TEAM_ID");
  if (!APPLE_ISSUER_PATTERN.test(issuer)) {
    throw new Error("APPLE_API_ISSUER must be a UUID.");
  }
  if (!isAbsolute(privateKeyPath)) {
    throw new Error("APPLE_API_KEY must be an absolute private-key file path.");
  }

  const entry = lstatSync(privateKeyPath);
  if (entry.isSymbolicLink() || !entry.isFile()) {
    throw new Error("APPLE_API_KEY must be a regular non-symlink file.");
  }
  if (entry.size < MIN_NOTARY_PRIVATE_KEY_BYTES || entry.size > MAX_NOTARY_PRIVATE_KEY_BYTES) {
    throw new Error("APPLE_API_KEY file size is outside the accepted private-key bounds.");
  }
  if ((entry.mode & 0o077) !== 0) {
    throw new Error("APPLE_API_KEY must not grant group or other permissions.");
  }
  const currentUid = input.currentUid ?? process.getuid?.();
  if (currentUid === undefined || entry.uid !== currentUid) {
    throw new Error("APPLE_API_KEY must be owned by the current build user.");
  }

  const privateKey = readFileSync(privateKeyPath, "utf8").trim();
  if (
    !privateKey.startsWith("-----BEGIN PRIVATE KEY-----") ||
    !privateKey.endsWith("-----END PRIVATE KEY-----")
  ) {
    throw new Error("APPLE_API_KEY must contain a PKCS#8 PEM private key.");
  }

  return {
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
  };
}
