// FILE: desktopControlPlaneProxySession.ts
// Purpose: Bind a Desktop Cloud Panel credential only to the local same-origin Control Plane proxy.
// Layer: Desktop main-process request policy

export function isDesktopControlPlaneProxyRequest(rawUrl: string, backendHttpUrl: string): boolean {
  if (!backendHttpUrl) return false;
  try {
    const request = new URL(rawUrl);
    const backend = new URL(backendHttpUrl);
    return (
      request.protocol === backend.protocol &&
      request.host === backend.host &&
      (request.pathname === "/v1" || request.pathname.startsWith("/v1/"))
    );
  } catch {
    return false;
  }
}

export function sanitizeDesktopControlPlaneProxyUrl(
  rawUrl: string,
  backendHttpUrl: string,
): string | null {
  if (!isDesktopControlPlaneProxyRequest(rawUrl, backendHttpUrl)) return null;
  const request = new URL(rawUrl);
  if (!request.searchParams.has("token")) return null;
  request.searchParams.delete("token");
  return request.toString();
}

export function buildDesktopControlPlaneProxyHeaders(
  requestHeaders: Readonly<Record<string, string | undefined>>,
  credential: string,
): Record<string, string> {
  const headers: Record<string, string> = {};
  for (const [name, value] of Object.entries(requestHeaders)) {
    if (
      value !== undefined &&
      name.toLowerCase() !== "cookie" &&
      name.toLowerCase() !== "authorization"
    ) {
      headers[name] = value;
    }
  }
  headers.Authorization = `Bearer ${credential}`;
  return headers;
}
