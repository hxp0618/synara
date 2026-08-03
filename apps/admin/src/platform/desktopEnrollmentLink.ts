// FILE: desktopEnrollmentLink.ts
// Purpose: Build the secret-bearing one-time Desktop link without persisting or logging it.
// Layer: Admin Platform domain

const HANDLE_PATTERN = /^[A-Za-z0-9_-]{43}$/;

function isLoopback(hostname: string): boolean {
  const normalized = hostname.toLowerCase();
  return normalized === "localhost" || normalized === "127.0.0.1" || normalized === "[::1]";
}

export function buildDesktopEnrollmentLink(input: {
  readonly controlPlaneOrigin: string;
  readonly handle: string;
  readonly scheme?: "synara" | "synara-canary";
}): string {
  const controlPlane = new URL(input.controlPlaneOrigin);
  if (
    controlPlane.username.length > 0 ||
    controlPlane.password.length > 0 ||
    controlPlane.search.length > 0 ||
    controlPlane.hash.length > 0 ||
    (controlPlane.protocol !== "https:" &&
      !(controlPlane.protocol === "http:" && isLoopback(controlPlane.hostname)))
  ) {
    throw new Error("The Control Plane URL is not eligible for Desktop Enrollment.");
  }
  if (!HANDLE_PATTERN.test(input.handle)) {
    throw new Error("The one-time Desktop Enrollment handle is invalid.");
  }
  const query = new URLSearchParams({
    v: "1",
    control_plane: controlPlane.toString().replace(/\/$/, ""),
    enrollment: input.handle,
  });
  return `${input.scheme ?? "synara"}://connect?${query.toString()}`;
}
