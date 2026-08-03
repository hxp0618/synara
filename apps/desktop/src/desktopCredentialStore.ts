// FILE: desktopCredentialStore.ts
// Purpose: Protect Desktop device keys and Cloud Panel credentials with Electron's OS credential boundary.
// Layer: Desktop main-process persistence

import * as Crypto from "node:crypto";
import * as FS from "node:fs";
import * as Path from "node:path";

const DOCUMENT_VERSION = 1;

export interface DesktopSafeStorage {
  isEncryptionAvailable(): boolean;
  encryptString(value: string): Buffer;
  decryptString(value: Buffer): string;
  getSelectedStorageBackend?(): string;
}

export type DesktopDeviceIdentity = {
  readonly publicKey: string;
  readonly privateKeyPkcs8: Buffer;
};

export type DesktopSaaSConnectionMetadata = {
  readonly controlPlaneBaseUrl: string;
  readonly sessionId: string;
  readonly credentialFamilyId: string;
  readonly credentialExpiresAt: string;
  readonly deviceId: string;
  readonly userId: string;
  readonly email: string;
  readonly displayName: string;
  readonly tenantId: string;
  readonly tenantName: string;
  readonly organizationId: string | null;
  readonly organizationName: string | null;
};

export type DesktopSaaSConnectionSecret = DesktopSaaSConnectionMetadata & {
  readonly credential: string;
};

type StoredDocument = {
  readonly version: 1;
  readonly device: {
    readonly publicKey: string;
    readonly encryptedPrivateKey: string;
  };
  readonly connection?: DesktopSaaSConnectionMetadata & {
    readonly encryptedCredential: string;
  };
};

type EncryptedConnectionPayload = {
  readonly credential: string;
  readonly controlPlaneBaseUrl: string;
  readonly sessionId: string;
  readonly credentialFamilyId: string;
  readonly deviceId: string;
  readonly userId: string;
  readonly tenantId: string;
};

export class DesktopCredentialStoreError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "DesktopCredentialStoreError";
  }
}

function validBase64Url(value: unknown, bytes: number): value is string {
  if (typeof value !== "string" || !/^[A-Za-z0-9_-]+$/u.test(value)) return false;
  try {
    return Buffer.from(value, "base64url").byteLength === bytes;
  } catch {
    return false;
  }
}

function requireString(value: unknown, field: string, maxLength = 2048): string {
  if (
    typeof value !== "string" ||
    value.length < 1 ||
    value.length > maxLength ||
    /[\r\n\u0000]/u.test(value)
  ) {
    throw new DesktopCredentialStoreError(
      "stored_connection_invalid",
      `Stored Desktop ${field} is invalid.`,
    );
  }
  return value;
}

function parseMetadata(
  value: unknown,
): DesktopSaaSConnectionMetadata & { encryptedCredential: string } {
  if (!value || typeof value !== "object") {
    throw new DesktopCredentialStoreError(
      "stored_connection_invalid",
      "Stored Desktop connection is invalid.",
    );
  }
  const record = value as Record<string, unknown>;
  return {
    controlPlaneBaseUrl: requireString(record.controlPlaneBaseUrl, "Control Plane URL"),
    sessionId: requireString(record.sessionId, "session ID", 80),
    credentialFamilyId: requireString(record.credentialFamilyId, "credential family", 80),
    credentialExpiresAt: requireString(record.credentialExpiresAt, "credential expiry", 80),
    deviceId: requireString(record.deviceId, "device ID", 80),
    userId: requireString(record.userId, "user ID", 80),
    email: requireString(record.email, "user email", 320),
    displayName: requireString(record.displayName, "display name", 160),
    tenantId: requireString(record.tenantId, "Tenant ID", 80),
    tenantName: requireString(record.tenantName, "Tenant name", 160),
    organizationId:
      record.organizationId === null
        ? null
        : requireString(record.organizationId, "Organization ID", 80),
    organizationName:
      record.organizationName === null
        ? null
        : requireString(record.organizationName, "Organization name", 160),
    encryptedCredential: requireString(record.encryptedCredential, "credential ciphertext", 16_384),
  };
}

export class DesktopCredentialStore {
  constructor(
    private readonly filePath: string,
    private readonly safeStorage: DesktopSafeStorage,
    private readonly platform: NodeJS.Platform = process.platform,
  ) {}

  assertAvailable(): void {
    let encryptionAvailable: boolean;
    try {
      encryptionAvailable = this.safeStorage.isEncryptionAvailable();
    } catch {
      throw new DesktopCredentialStoreError(
        "os_credential_store_unavailable",
        "The operating-system credential store is unavailable. Desktop connection was not attempted.",
      );
    }
    if (!encryptionAvailable) {
      throw new DesktopCredentialStoreError(
        "os_credential_store_unavailable",
        "The operating-system credential store is unavailable. Desktop connection was not attempted.",
      );
    }
    let selectedBackend: string | undefined;
    try {
      selectedBackend = this.safeStorage.getSelectedStorageBackend?.();
    } catch {
      throw new DesktopCredentialStoreError(
        "os_credential_store_unavailable",
        "The operating-system credential store is unavailable. Desktop connection was not attempted.",
      );
    }
    if (this.platform === "linux" && selectedBackend?.trim().toLowerCase() === "basic_text") {
      throw new DesktopCredentialStoreError(
        "os_credential_store_insecure",
        "A supported Linux Secret Service is required for Desktop connection.",
      );
    }
  }

  ensureDeviceIdentity(): DesktopDeviceIdentity {
    this.assertAvailable();
    const existing = this.readDocument();
    if (existing) return this.decryptDevice(existing);

    const { publicKey, privateKey } = Crypto.generateKeyPairSync("ed25519");
    const publicKeyDer = publicKey.export({ type: "spki", format: "der" });
    const privateKeyDer = privateKey.export({ type: "pkcs8", format: "der" });
    const rawPublicKey = publicKeyDer.subarray(publicKeyDer.length - 32);
    if (rawPublicKey.byteLength !== 32) {
      throw new DesktopCredentialStoreError(
        "device_key_generation_failed",
        "Desktop device identity could not be generated.",
      );
    }
    const document: StoredDocument = {
      version: DOCUMENT_VERSION,
      device: {
        publicKey: rawPublicKey.toString("base64url"),
        encryptedPrivateKey: this.safeStorage
          .encryptString(privateKeyDer.toString("base64"))
          .toString("base64"),
      },
    };
    this.writeDocument(document);
    return { publicKey: document.device.publicKey, privateKeyPkcs8: Buffer.from(privateKeyDer) };
  }

  readConnection(): DesktopSaaSConnectionSecret | null {
    const document = this.readDocument();
    if (!document?.connection) return null;
    this.assertAvailable();
    const connection = parseMetadata(document.connection);
    let payload: EncryptedConnectionPayload;
    try {
      payload = JSON.parse(
        this.safeStorage.decryptString(Buffer.from(connection.encryptedCredential, "base64")),
      ) as EncryptedConnectionPayload;
    } catch {
      throw new DesktopCredentialStoreError(
        "stored_connection_decrypt_failed",
        "The saved Desktop connection could not be unlocked.",
      );
    }
    const credential = payload?.credential;
    if (
      typeof credential !== "string" ||
      credential.length < 32 ||
      credential.length > 4096 ||
      /\s/u.test(credential)
    ) {
      throw new DesktopCredentialStoreError(
        "stored_connection_invalid",
        "The saved Desktop credential is invalid.",
      );
    }
    const bindingMatches =
      payload.controlPlaneBaseUrl === connection.controlPlaneBaseUrl &&
      payload.sessionId === connection.sessionId &&
      payload.credentialFamilyId === connection.credentialFamilyId &&
      payload.deviceId === connection.deviceId &&
      payload.userId === connection.userId &&
      payload.tenantId === connection.tenantId;
    if (!bindingMatches) {
      throw new DesktopCredentialStoreError(
        "stored_connection_binding_mismatch",
        "The saved Desktop connection metadata failed its protected binding check.",
      );
    }
    const { encryptedCredential: _encryptedCredential, ...metadata } = connection;
    return { ...metadata, credential };
  }

  saveConnection(connection: DesktopSaaSConnectionSecret): void {
    this.assertAvailable();
    const document = this.readDocument();
    if (!document) {
      throw new DesktopCredentialStoreError(
        "device_identity_missing",
        "Desktop device identity is missing.",
      );
    }
    const { credential, ...metadata } = connection;
    if (credential.length < 32 || credential.length > 4096 || /\s/u.test(credential)) {
      throw new DesktopCredentialStoreError(
        "desktop_credential_invalid",
        "Desktop credential is invalid.",
      );
    }
    this.writeDocument({
      ...document,
      connection: {
        ...metadata,
        encryptedCredential: this.safeStorage
          .encryptString(
            JSON.stringify({
              credential,
              controlPlaneBaseUrl: metadata.controlPlaneBaseUrl,
              sessionId: metadata.sessionId,
              credentialFamilyId: metadata.credentialFamilyId,
              deviceId: metadata.deviceId,
              userId: metadata.userId,
              tenantId: metadata.tenantId,
            } satisfies EncryptedConnectionPayload),
          )
          .toString("base64"),
      },
    });
  }

  clearAll(): void {
    try {
      FS.unlinkSync(this.filePath);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw new DesktopCredentialStoreError(
          "stored_connection_clear_failed",
          "The saved Desktop connection could not be cleared.",
        );
      }
    }
  }

  private decryptDevice(document: StoredDocument): DesktopDeviceIdentity {
    if (!validBase64Url(document.device.publicKey, 32)) {
      throw new DesktopCredentialStoreError(
        "stored_device_invalid",
        "The saved Desktop device identity is invalid.",
      );
    }
    let privateKeyPkcs8: Buffer;
    try {
      privateKeyPkcs8 = Buffer.from(
        this.safeStorage.decryptString(Buffer.from(document.device.encryptedPrivateKey, "base64")),
        "base64",
      );
      const privateKey = Crypto.createPrivateKey({
        key: privateKeyPkcs8,
        type: "pkcs8",
        format: "der",
      });
      const derivedPublicKey = Crypto.createPublicKey(privateKey).export({
        type: "spki",
        format: "der",
      });
      if (
        !derivedPublicKey
          .subarray(derivedPublicKey.length - 32)
          .equals(Buffer.from(document.device.publicKey, "base64url"))
      ) {
        throw new Error("stored key pair mismatch");
      }
    } catch {
      throw new DesktopCredentialStoreError(
        "stored_device_decrypt_failed",
        "The saved Desktop device identity could not be unlocked.",
      );
    }
    return { publicKey: document.device.publicKey, privateKeyPkcs8 };
  }

  private readDocument(): StoredDocument | null {
    if (!FS.existsSync(this.filePath)) return null;
    let parsed: unknown;
    try {
      parsed = JSON.parse(FS.readFileSync(this.filePath, "utf8"));
    } catch {
      throw new DesktopCredentialStoreError(
        "stored_connection_invalid",
        "The saved Desktop connection is unreadable.",
      );
    }
    if (!parsed || typeof parsed !== "object") {
      throw new DesktopCredentialStoreError(
        "stored_connection_invalid",
        "The saved Desktop connection is invalid.",
      );
    }
    const record = parsed as Record<string, unknown>;
    const device = record.device as Record<string, unknown> | undefined;
    if (
      record.version !== DOCUMENT_VERSION ||
      !device ||
      !validBase64Url(device.publicKey, 32) ||
      typeof device.encryptedPrivateKey !== "string" ||
      device.encryptedPrivateKey.length < 1 ||
      device.encryptedPrivateKey.length > 16_384
    ) {
      throw new DesktopCredentialStoreError(
        "stored_connection_invalid",
        "The saved Desktop connection is invalid.",
      );
    }
    return {
      version: DOCUMENT_VERSION,
      device: {
        publicKey: device.publicKey,
        encryptedPrivateKey: device.encryptedPrivateKey,
      },
      ...(record.connection === undefined ? {} : { connection: parseMetadata(record.connection) }),
    };
  }

  private writeDocument(document: StoredDocument): void {
    const directory = Path.dirname(this.filePath);
    FS.mkdirSync(directory, { recursive: true, mode: 0o700 });
    if (this.platform !== "win32") FS.chmodSync(directory, 0o700);
    const temporaryPath = `${this.filePath}.${process.pid}.${Crypto.randomBytes(8).toString("hex")}.tmp`;
    let descriptor: number | null = null;
    try {
      descriptor = FS.openSync(temporaryPath, "wx", 0o600);
      FS.writeFileSync(descriptor, `${JSON.stringify(document)}\n`, { encoding: "utf8" });
      FS.fsyncSync(descriptor);
      FS.closeSync(descriptor);
      descriptor = null;
      if (this.platform !== "win32") FS.chmodSync(temporaryPath, 0o600);
      FS.renameSync(temporaryPath, this.filePath);
    } catch {
      if (descriptor !== null) FS.closeSync(descriptor);
      try {
        FS.unlinkSync(temporaryPath);
      } catch {
        // The failed temporary file is already absent.
      }
      throw new DesktopCredentialStoreError(
        "stored_connection_write_failed",
        "The Desktop connection could not be saved safely.",
      );
    }
  }
}
