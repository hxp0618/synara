// FILE: desktopEnrollmentLink.ts
// Purpose: Parse the one documented secret-bearing Desktop Enrollment deep link fail closed.
// Layer: Desktop main-process security boundary

const MAX_LINK_LENGTH = 4096;
const HANDLE_PATTERN = /^[A-Za-z0-9_-]{43}$/;
const EXPECTED_PARAMETERS = new Set(["v", "control_plane", "enrollment"]);

export type DesktopEnrollmentLink = {
  readonly controlPlaneBaseUrl: string;
  readonly handle: string;
};

export class DesktopEnrollmentLinkError extends Error {
  constructor(readonly code: string) {
    super("The Desktop connection link is invalid or is not allowed by this Synara build.");
    this.name = "DesktopEnrollmentLinkError";
  }
}

function isLoopback(hostname: string): boolean {
  const normalized = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  return normalized === "localhost" || normalized === "127.0.0.1" || normalized === "::1";
}

export function normalizeDesktopControlPlaneBaseUrl(
  rawUrl: string,
  allowLoopbackHttp: boolean,
): string | null {
  let parsed: URL;
  try {
    parsed = new URL(rawUrl);
  } catch {
    return null;
  }
  if (
    parsed.username.length > 0 ||
    parsed.password.length > 0 ||
    parsed.search.length > 0 ||
    parsed.hash.length > 0 ||
    (parsed.protocol !== "https:" &&
      !(allowLoopbackHttp && parsed.protocol === "http:" && isLoopback(parsed.hostname)))
  ) {
    return null;
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, "");
  return parsed.toString().replace(/\/$/, "");
}

export function parseDesktopEnrollmentOriginAllowlist(input: {
  readonly raw?: string | undefined;
  readonly allowLoopbackHttp: boolean;
}): ReadonlySet<string> {
  const allowed = new Set<string>();
  for (const candidate of input.raw?.split(",") ?? []) {
    const normalized = normalizeDesktopControlPlaneBaseUrl(
      candidate.trim(),
      input.allowLoopbackHttp,
    );
    if (normalized) allowed.add(normalized);
  }
  return allowed;
}

export function parseDesktopEnrollmentLink(
  rawLink: string,
  input: {
    readonly scheme: string;
    readonly allowedControlPlaneBaseUrls: ReadonlySet<string>;
    readonly allowLoopbackHttp: boolean;
  },
): DesktopEnrollmentLink {
  if (
    typeof rawLink !== "string" ||
    rawLink.length < 1 ||
    rawLink.length > MAX_LINK_LENGTH ||
    /[\r\n\u0000]/u.test(rawLink)
  ) {
    throw new DesktopEnrollmentLinkError("malformed_link");
  }
  let link: URL;
  try {
    link = new URL(rawLink);
  } catch {
    throw new DesktopEnrollmentLinkError("malformed_link");
  }
  if (
    link.protocol !== `${input.scheme}:` ||
    link.hostname !== "connect" ||
    (link.pathname !== "" && link.pathname !== "/") ||
    link.username.length > 0 ||
    link.password.length > 0 ||
    link.hash.length > 0
  ) {
    throw new DesktopEnrollmentLinkError("route_not_allowed");
  }
  for (const key of link.searchParams.keys()) {
    if (!EXPECTED_PARAMETERS.has(key) || link.searchParams.getAll(key).length !== 1) {
      throw new DesktopEnrollmentLinkError("parameters_not_allowed");
    }
  }
  if ([...EXPECTED_PARAMETERS].some((key) => link.searchParams.getAll(key).length !== 1)) {
    throw new DesktopEnrollmentLinkError("parameters_missing");
  }
  if (link.searchParams.get("v") !== "1") {
    throw new DesktopEnrollmentLinkError("version_not_supported");
  }
  const controlPlaneBaseUrl = normalizeDesktopControlPlaneBaseUrl(
    link.searchParams.get("control_plane") ?? "",
    input.allowLoopbackHttp,
  );
  if (!controlPlaneBaseUrl || !input.allowedControlPlaneBaseUrls.has(controlPlaneBaseUrl)) {
    throw new DesktopEnrollmentLinkError("control_plane_not_allowed");
  }
  const handle = link.searchParams.get("enrollment") ?? "";
  if (!HANDLE_PATTERN.test(handle)) {
    throw new DesktopEnrollmentLinkError("handle_invalid");
  }
  try {
    if (Buffer.from(handle, "base64url").byteLength !== 32) {
      throw new Error("invalid handle length");
    }
  } catch {
    throw new DesktopEnrollmentLinkError("handle_invalid");
  }
  return { controlPlaneBaseUrl, handle };
}

export function findDesktopEnrollmentLinkArgument(
  argv: ReadonlyArray<string>,
  scheme: string,
): string | null {
  const prefix = `${scheme}://connect`;
  return argv.find((argument) => argument.startsWith(prefix)) ?? null;
}
