// FILE: controlPlaneProxyTarget.mjs
// Purpose: Preserve a fixed Control Plane deployment path while proxying Admin requests.
// Layer: Admin production runtime helper

export function resolveControlPlaneProxyTarget(controlPlane, requestPath) {
  const incoming = new URL(requestPath, "http://admin.local");
  const target = new URL(controlPlane);
  const basePath = target.pathname.replace(/\/+$/, "");
  target.pathname = `${basePath}${incoming.pathname}`;
  target.search = incoming.search;
  target.hash = "";
  return target;
}
