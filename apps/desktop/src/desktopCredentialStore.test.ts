import * as FS from "node:fs";
import * as OS from "node:os";
import * as Path from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import {
  DesktopCredentialStore,
  DesktopCredentialStoreError,
  type DesktopSafeStorage,
} from "./desktopCredentialStore";

const temporaryDirectories: string[] = [];

function tempFile(): string {
  const directory = FS.mkdtempSync(Path.join(OS.tmpdir(), "synara-desktop-credential-test-"));
  temporaryDirectories.push(directory);
  return Path.join(directory, "desktop-saas-connection.json");
}

function fakeSafeStorage(backend = "keychain"): DesktopSafeStorage {
  return {
    isEncryptionAvailable: () => true,
    getSelectedStorageBackend: () => backend,
    encryptString: (value) => Buffer.from(`cipher:${Buffer.from(value).toString("base64")}`),
    decryptString: (value) =>
      Buffer.from(value.toString().slice("cipher:".length), "base64").toString(),
  };
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    FS.rmSync(directory, { recursive: true, force: true });
  }
});

describe("DesktopCredentialStore", () => {
  it("does not open the OS credential store when no saved connection exists", () => {
    let availabilityChecks = 0;
    const safeStorage: DesktopSafeStorage = {
      ...fakeSafeStorage(),
      isEncryptionAvailable: () => {
        availabilityChecks += 1;
        throw new Error("credential store should stay untouched");
      },
    };
    const store = new DesktopCredentialStore(tempFile(), safeStorage, "darwin");

    expect(store.readConnection()).toBeNull();
    expect(availabilityChecks).toBe(0);
  });

  it("persists only OS-encrypted key and credential material", () => {
    const filePath = tempFile();
    const store = new DesktopCredentialStore(filePath, fakeSafeStorage(), "darwin");
    const identity = store.ensureDeviceIdentity();
    const credential = "desktop-credential-that-must-never-be-plaintext";
    store.saveConnection({
      controlPlaneBaseUrl: "https://control.example.com",
      sessionId: "session-1",
      credentialFamilyId: "family-1",
      credentialExpiresAt: "2026-08-30T00:00:00Z",
      deviceId: "device-1",
      userId: "user-1",
      email: "owner@example.com",
      displayName: "Owner",
      tenantId: "tenant-1",
      tenantName: "Northstar Labs",
      organizationId: null,
      organizationName: null,
      credential,
    });

    const stored = FS.readFileSync(filePath, "utf8");
    expect(stored).not.toContain(credential);
    expect(stored).not.toContain(identity.privateKeyPkcs8.toString("base64"));
    expect(FS.statSync(filePath).mode & 0o777).toBe(0o600);
    expect(store.readConnection()).toEqual(
      expect.objectContaining({ credential, deviceId: "device-1" }),
    );
    expect(store.ensureDeviceIdentity().publicKey).toBe(identity.publicKey);
  });

  it("fails closed when Linux falls back to basic_text", () => {
    const store = new DesktopCredentialStore(tempFile(), fakeSafeStorage("basic_text"), "linux");
    expect(() => store.ensureDeviceIdentity()).toThrowError(
      expect.objectContaining<Partial<DesktopCredentialStoreError>>({
        code: "os_credential_store_insecure",
      }),
    );
  });

  it("rejects plaintext metadata tampering before a credential can be used", () => {
    const filePath = tempFile();
    const store = new DesktopCredentialStore(filePath, fakeSafeStorage(), "darwin");
    store.ensureDeviceIdentity();
    store.saveConnection({
      controlPlaneBaseUrl: "https://control.example.com",
      sessionId: "session-1",
      credentialFamilyId: "family-1",
      credentialExpiresAt: "2026-08-30T00:00:00Z",
      deviceId: "device-1",
      userId: "user-1",
      email: "owner@example.com",
      displayName: "Owner",
      tenantId: "tenant-1",
      tenantName: "Northstar Labs",
      organizationId: null,
      organizationName: null,
      credential: "desktop-credential-that-must-never-be-plaintext",
    });
    const document = JSON.parse(FS.readFileSync(filePath, "utf8")) as Record<string, unknown>;
    const connection = document.connection as Record<string, unknown>;
    connection.controlPlaneBaseUrl = "https://attacker.example";
    FS.writeFileSync(filePath, `${JSON.stringify(document)}\n`);

    expect(() => store.readConnection()).toThrowError(
      expect.objectContaining<Partial<DesktopCredentialStoreError>>({
        code: "stored_connection_binding_mismatch",
      }),
    );
  });

  it("clears the exact connection document without touching its directory", () => {
    const filePath = tempFile();
    const store = new DesktopCredentialStore(filePath, fakeSafeStorage(), "darwin");
    store.ensureDeviceIdentity();
    const sibling = Path.join(Path.dirname(filePath), "keep.txt");
    FS.writeFileSync(sibling, "keep");

    store.clearAll();

    expect(FS.existsSync(filePath)).toBe(false);
    expect(FS.readFileSync(sibling, "utf8")).toBe("keep");
  });
});
