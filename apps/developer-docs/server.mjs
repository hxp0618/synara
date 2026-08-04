import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { createServer } from "node:http";
import { dirname, extname, resolve, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const moduleDirectory = dirname(fileURLToPath(import.meta.url));

const contentTypes = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".ico", "image/x-icon"],
  [".js", "text/javascript; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".mjs", "text/javascript; charset=utf-8"],
  [".png", "image/png"],
  [".svg", "image/svg+xml"],
  [".txt", "text/plain; charset=utf-8"],
  [".webp", "image/webp"],
  [".woff", "font/woff"],
  [".woff2", "font/woff2"],
  [".yaml", "application/yaml; charset=utf-8"],
  [".yml", "application/yaml; charset=utf-8"],
]);

function applySecurityHeaders(response) {
  response.setHeader("Cross-Origin-Opener-Policy", "same-origin");
  response.setHeader("Referrer-Policy", "strict-origin-when-cross-origin");
  response.setHeader("X-Content-Type-Options", "nosniff");
  response.setHeader("X-Frame-Options", "DENY");
}

function sendText(response, status, body, method = "GET") {
  const encoded = Buffer.from(body);
  applySecurityHeaders(response);
  response.writeHead(status, {
    "Cache-Control": "no-store",
    "Content-Length": encoded.byteLength,
    "Content-Type": "text/plain; charset=utf-8",
  });
  response.end(method === "HEAD" ? undefined : encoded);
}

export function resolveStaticRequest(root, rawPathname) {
  let pathname;
  try {
    pathname = decodeURIComponent(rawPathname);
  } catch {
    throw new Error("invalid URL encoding");
  }
  const segments = pathname.split("/");
  if (
    pathname.includes("\0") ||
    pathname.includes("\\") ||
    segments.some((segment) => segment === ".." || segment.startsWith("."))
  ) {
    throw new Error("unsafe static path");
  }
  const rootPath = resolve(root);
  const candidate = resolve(rootPath, `.${pathname}`);
  if (candidate !== rootPath && !candidate.startsWith(`${rootPath}${sep}`)) {
    throw new Error("static path escapes the document root");
  }
  return candidate;
}

async function regularFile(pathname) {
  try {
    const metadata = await stat(pathname);
    return metadata.isFile() ? metadata : null;
  } catch (error) {
    if (error?.code === "ENOENT" || error?.code === "ENOTDIR") return null;
    throw error;
  }
}

async function directory(pathname) {
  try {
    return (await stat(pathname)).isDirectory();
  } catch (error) {
    if (error?.code === "ENOENT" || error?.code === "ENOTDIR") return false;
    throw error;
  }
}

export function createDeveloperDocsServer(options = {}) {
  const root = resolve(
    options.root ?? process.env.POLARIS_DEVELOPER_DOCS_ROOT ?? resolve(moduleDirectory, "dist"),
  );
  const revision = options.revision ?? process.env.POLARIS_DEVELOPER_DOCS_REVISION ?? "unknown";

  return createServer(async (request, response) => {
    const method = request.method ?? "GET";
    if (method !== "GET" && method !== "HEAD") {
      response.setHeader("Allow", "GET, HEAD");
      sendText(response, 405, "Method Not Allowed\n", method);
      return;
    }

    let url;
    try {
      url = new URL(request.url ?? "/", "http://developer-docs.invalid");
    } catch {
      sendText(response, 400, "Bad Request\n", method);
      return;
    }

    if (url.pathname === "/ready") {
      const body = Buffer.from(`${JSON.stringify({ status: "ready", revision })}\n`);
      applySecurityHeaders(response);
      response.writeHead(200, {
        "Cache-Control": "no-store",
        "Content-Length": body.byteLength,
        "Content-Type": "application/json; charset=utf-8",
      });
      response.end(method === "HEAD" ? undefined : body);
      return;
    }

    try {
      let pathname = resolveStaticRequest(root, url.pathname);
      if (await directory(pathname)) {
        if (!url.pathname.endsWith("/")) {
          applySecurityHeaders(response);
          response.writeHead(308, { Location: `${url.pathname}/${url.search}` });
          response.end();
          return;
        }
        pathname = resolve(pathname, "index.html");
      }
      const metadata = await regularFile(pathname);
      if (!metadata) {
        sendText(response, 404, "Not Found\n", method);
        return;
      }

      const etag = `W/\"${metadata.size.toString(16)}-${Math.trunc(metadata.mtimeMs).toString(16)}\"`;
      applySecurityHeaders(response);
      response.setHeader(
        "Cache-Control",
        url.pathname.startsWith("/_astro/")
          ? "public, max-age=31536000, immutable"
          : extname(pathname) === ".html"
            ? "no-cache"
            : "public, max-age=300",
      );
      response.setHeader("ETag", etag);
      if (request.headers["if-none-match"] === etag) {
        response.writeHead(304);
        response.end();
        return;
      }
      response.setHeader("Content-Length", metadata.size);
      response.setHeader(
        "Content-Type",
        contentTypes.get(extname(pathname).toLowerCase()) ?? "application/octet-stream",
      );
      response.writeHead(200);
      if (method === "HEAD") {
        response.end();
        return;
      }
      createReadStream(pathname)
        .on("error", () => response.destroy())
        .pipe(response);
    } catch (error) {
      if (
        error?.message === "invalid URL encoding" ||
        error?.message === "unsafe static path" ||
        error?.message === "static path escapes the document root"
      ) {
        sendText(response, 400, "Bad Request\n", method);
        return;
      }
      sendText(response, 500, "Internal Server Error\n", method);
    }
  });
}

function parsePort(value) {
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65_535) {
    throw new Error("POLARIS_DEVELOPER_DOCS_PORT must be an integer from 1 through 65535");
  }
  return port;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const host = process.env.POLARIS_DEVELOPER_DOCS_HOST ?? "0.0.0.0";
  const port = parsePort(process.env.POLARIS_DEVELOPER_DOCS_PORT ?? "8080");
  const server = createDeveloperDocsServer();
  server.listen(port, host, () => {
    console.log(`Polaris Developer Docs listening on http://${host}:${port}`);
  });
}
