// FILE: server.mjs
// Purpose: Serve the built Admin app and same-origin proxy authenticated /v1 Control Plane requests.
// Layer: Admin production runtime

import { createReadStream } from "node:fs";
import { access, stat } from "node:fs/promises";
import http from "node:http";
import https from "node:https";
import path from "node:path";
import { pipeline } from "node:stream";
import { fileURLToPath } from "node:url";

import { resolveControlPlaneProxyTarget } from "./controlPlaneProxyTarget.mjs";

const host = process.env.SYNARA_ADMIN_HOST?.trim() || "0.0.0.0";
const port = Number(process.env.SYNARA_ADMIN_PORT || 3774);
const controlPlane = new URL(
  process.env.SYNARA_CONTROL_PLANE_URL?.trim() || "http://127.0.0.1:3780",
);
const tenantAppUrl = process.env.SYNARA_TENANT_APP_URL?.trim() || "/";
const staticRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "dist");
const hopByHopHeaders = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);
const contentTypes = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".ico", "image/x-icon"],
  [".js", "text/javascript; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".png", "image/png"],
  [".svg", "image/svg+xml"],
  [".webp", "image/webp"],
]);

function applySecurityHeaders(response) {
  response.setHeader(
    "Content-Security-Policy",
    "default-src 'none'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; script-src 'self'; style-src 'self'",
  );
  response.setHeader("Cross-Origin-Opener-Policy", "same-origin");
  response.setHeader("Permissions-Policy", "camera=(), microphone=(), geolocation=()");
  response.setHeader("Referrer-Policy", "no-referrer");
  response.setHeader("X-Content-Type-Options", "nosniff");
  response.setHeader("X-Frame-Options", "DENY");
}

function filteredHeaders(headers) {
  return Object.fromEntries(
    Object.entries(headers).filter(
      ([name, value]) => value !== undefined && !hopByHopHeaders.has(name.toLowerCase()),
    ),
  );
}

function proxyControlPlane(request, response, requestPath) {
  const target = resolveControlPlaneProxyTarget(controlPlane, requestPath);
  const headers = filteredHeaders(request.headers);
  headers.host = controlPlane.host;
  headers["x-forwarded-host"] = request.headers.host || "";
  headers["x-forwarded-proto"] = request.socket.encrypted ? "https" : "http";

  const upstreamClient = target.protocol === "https:" ? https : http;
  const upstream = upstreamClient.request(
    target,
    { method: request.method, headers, timeout: 30_000 },
    (upstreamResponse) => {
      response.writeHead(
        upstreamResponse.statusCode || 502,
        filteredHeaders(upstreamResponse.headers),
      );
      pipeline(upstreamResponse, response, (error) => {
        if (error && !response.destroyed) response.destroy(error);
      });
    },
  );
  upstream.on("timeout", () => upstream.destroy(new Error("Control Plane request timed out.")));
  upstream.on("error", () => {
    if (response.headersSent) {
      response.destroy();
      return;
    }
    response.writeHead(502, { "content-type": "application/json; charset=utf-8" });
    response.end(
      JSON.stringify({
        error: {
          code: "control_plane_unavailable",
          message: "Control Plane is unavailable.",
        },
      }),
    );
  });
  pipeline(request, upstream, (error) => {
    if (error && !upstream.destroyed) upstream.destroy(error);
  });
}

async function staticFile(request, response, pathname) {
  if (request.method !== "GET" && request.method !== "HEAD") {
    response.writeHead(405, { allow: "GET, HEAD" });
    response.end();
    return;
  }
  let decoded;
  try {
    decoded = decodeURIComponent(pathname);
  } catch {
    response.writeHead(400);
    response.end("Invalid path");
    return;
  }
  if (decoded.includes("\0")) {
    response.writeHead(400);
    response.end("Invalid path");
    return;
  }
  const relative = decoded === "/" ? "index.html" : decoded.replace(/^\/+/, "");
  let candidate = path.resolve(staticRoot, relative);
  if (candidate !== staticRoot && !candidate.startsWith(`${staticRoot}${path.sep}`)) {
    response.writeHead(400);
    response.end("Invalid path");
    return;
  }
  try {
    const metadata = await stat(candidate);
    if (metadata.isDirectory()) candidate = path.join(candidate, "index.html");
    await access(candidate);
  } catch {
    candidate = path.join(staticRoot, "index.html");
  }
  const extension = path.extname(candidate).toLowerCase();
  applySecurityHeaders(response);
  response.setHeader("Content-Type", contentTypes.get(extension) || "application/octet-stream");
  response.setHeader(
    "Cache-Control",
    path.basename(candidate) === "index.html" ? "no-store" : "public, max-age=31536000, immutable",
  );
  if (request.method === "HEAD") {
    response.writeHead(200);
    response.end();
    return;
  }
  response.writeHead(200);
  pipeline(createReadStream(candidate), response, (error) => {
    if (error && !response.destroyed) response.destroy(error);
  });
}

const server = http.createServer((request, response) => {
  const url = new URL(request.url || "/", `http://${request.headers.host || "localhost"}`);
  if (url.pathname === "/ready") {
    response.writeHead(200, {
      "cache-control": "no-store",
      "content-type": "application/json; charset=utf-8",
    });
    response.end(JSON.stringify({ ready: true, surface: "platform-admin" }));
    return;
  }
  if (url.pathname === "/admin-config.json") {
    applySecurityHeaders(response);
    response.writeHead(200, {
      "cache-control": "no-store",
      "content-type": "application/json; charset=utf-8",
    });
    response.end(JSON.stringify({ tenantAppUrl }));
    return;
  }
  if (url.pathname === "/v1" || url.pathname.startsWith("/v1/")) {
    proxyControlPlane(request, response, `${url.pathname}${url.search}`);
    return;
  }
  void staticFile(request, response, url.pathname).catch(() => {
    if (!response.headersSent) response.writeHead(500);
    response.end("Admin application could not be served.");
  });
});

server.headersTimeout = 35_000;
server.requestTimeout = 35_000;
server.listen(port, host, () => {
  console.info(`Synara Platform Admin listening on http://${host}:${port}`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => server.close(() => process.exit(0)));
}
