import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { request } from "node:http";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { Server } from "node:http";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { createDeveloperDocsServer, resolveStaticRequest } from "../server.mjs";

describe("Developer Docs production server", () => {
  let origin = "";
  let root = "";
  let server: Server;

  beforeAll(async () => {
    root = await mkdtemp(join(tmpdir(), "polaris-developer-docs-test-"));
    server = createDeveloperDocsServer({ root, revision: "test-revision" });
    await mkdir(join(root, "guide"), { recursive: true });
    await mkdir(join(root, "_astro"), { recursive: true });
    await writeFile(join(root, "index.html"), "<h1>Polaris</h1>");
    await writeFile(join(root, "guide", "index.html"), "<h1>Guide</h1>");
    await writeFile(join(root, "_astro", "app.js"), "export {};");
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("test server has no TCP address");
    origin = `http://127.0.0.1:${address.port}`;
  });

  afterAll(async () => {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
    await rm(root, { recursive: true, force: true });
  });

  it("serves HTML, readiness and immutable assets with bounded headers", async () => {
    const home = await fetch(`${origin}/`);
    expect(home.status).toBe(200);
    expect(await home.text()).toBe("<h1>Polaris</h1>");
    expect(home.headers.get("cache-control")).toBe("no-cache");
    expect(home.headers.get("x-content-type-options")).toBe("nosniff");

    const ready = await fetch(`${origin}/ready`);
    expect(await ready.json()).toEqual({ status: "ready", revision: "test-revision" });
    expect(ready.headers.get("cache-control")).toBe("no-store");

    const asset = await fetch(`${origin}/_astro/app.js`);
    expect(asset.status).toBe(200);
    expect(asset.headers.get("cache-control")).toBe("public, max-age=31536000, immutable");
  });

  it("canonicalizes directory URLs and supports HEAD plus conditional requests", async () => {
    const redirect = await fetch(`${origin}/guide?source=test`, { redirect: "manual" });
    expect(redirect.status).toBe(308);
    expect(redirect.headers.get("location")).toBe("/guide/?source=test");

    const head = await fetch(`${origin}/guide/`, { method: "HEAD" });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");

    const first = await fetch(`${origin}/guide/`);
    const cached = await fetch(`${origin}/guide/`, {
      headers: { "If-None-Match": first.headers.get("etag") ?? "" },
    });
    expect(cached.status).toBe(304);
  });

  it("fails closed for methods, missing files and unsafe paths", async () => {
    expect((await fetch(`${origin}/missing`)).status).toBe(404);
    expect((await fetch(`${origin}/`, { method: "POST" })).status).toBe(405);
    expect(() => resolveStaticRequest(root, "/../secret")).toThrow("unsafe static path");
    expect(() => resolveStaticRequest(root, "/.hidden")).toThrow("unsafe static path");

    const malformedStatus = await new Promise<number>((resolve, reject) => {
      const client = request(`${origin}/%E0%A4%A`, (response) => {
        response.resume();
        resolve(response.statusCode ?? 0);
      });
      client.on("error", reject);
      client.end();
    });
    expect(malformedStatus).toBe(400);
  });
});
