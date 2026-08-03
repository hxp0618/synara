import { EventEmitter } from "node:events";

import { Effect, Stream } from "effect";
import { describe, expect, it } from "vitest";

import {
  bindControlPlaneProxyAbort,
  buildControlPlaneProxyCorsHeaders,
  buildControlPlaneProxyRequestHeaders,
  buildControlPlaneProxyResponseHeaders,
  resolveControlPlaneTarget,
  shouldStreamControlPlaneResponse,
  streamControlPlaneResponseBody,
} from "./controlPlaneProxy";
import type { ServerConfigShape } from "./config";

describe("resolveControlPlaneTarget", () => {
  const localDesktopConfig = {
    host: "127.0.0.1",
    publicUrl: undefined,
    devUrl: undefined,
  } as ServerConfigShape;

  it("keeps the public path and query while replacing the internal origin", () => {
    expect(
      resolveControlPlaneTarget(
        new URL("http://control-plane:3780"),
        new URL("https://synara.example/v1/tenants?limit=25"),
      ).toString(),
    ).toBe("http://control-plane:3780/v1/tenants?limit=25");
  });

  it("preserves SCIM paths for enterprise directory providers", () => {
    expect(
      resolveControlPlaneTarget(
        new URL("http://control-plane:3780"),
        new URL("https://synara.example/scim/v2/Users?count=100"),
      ).toString(),
    ).toBe("http://control-plane:3780/scim/v2/Users?count=100");
  });

  it("preserves a fixed Control Plane deployment base path", () => {
    expect(
      resolveControlPlaneTarget(
        new URL("https://gateway.example/control-plane/"),
        new URL("http://127.0.0.1:58180/v1/auth/session?refresh=1"),
      ).toString(),
    ).toBe("https://gateway.example/control-plane/v1/auth/session?refresh=1");
  });

  it("streams event-stream responses without buffering them", () => {
    const response = new Response(new ReadableStream(), {
      headers: { "Content-Type": "text/event-stream; charset=utf-8" },
    });
    expect(shouldStreamControlPlaneResponse(response)).toBe(true);
    expect(shouldStreamControlPlaneResponse(new Response("{}"))).toBe(false);
  });

  it("streams audit and Artifact attachments without buffering them", () => {
    const response = new Response(new ReadableStream(), {
      headers: {
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": 'attachment; filename="audit.csv"',
      },
    });
    expect(shouldStreamControlPlaneResponse(response)).toBe(true);
  });

  it("cancels the upstream body when downstream streaming stops", async () => {
    let cancelCount = 0;
    let finalizeCount = 0;
    const upstreamAbort = new AbortController();
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.enqueue(Uint8Array.of(1));
      },
      cancel() {
        cancelCount += 1;
      },
    });

    await Effect.runPromise(
      streamControlPlaneResponseBody(body, () => {
        finalizeCount += 1;
        upstreamAbort.abort();
      }).pipe(Stream.take(1), Stream.runDrain),
    );

    expect(cancelCount).toBe(1);
    expect(finalizeCount).toBe(1);
    expect(upstreamAbort.signal.aborted).toBe(true);
  });

  it("aborts the upstream request when the downstream request aborts", () => {
    const request = Object.assign(new EventEmitter(), { complete: false });
    const response = Object.assign(new EventEmitter(), { writableEnded: false });
    const upstreamAbort = new AbortController();
    const finalize = bindControlPlaneProxyAbort({ request, response }, upstreamAbort);

    request.emit("aborted");

    expect(upstreamAbort.signal.aborted).toBe(true);
    finalize();
    expect(request.listenerCount("aborted")).toBe(0);
    expect(request.listenerCount("close")).toBe(0);
    expect(response.listenerCount("close")).toBe(0);
  });

  it("forwards cookies while replacing untrusted forwarded headers", () => {
    const headers = buildControlPlaneProxyRequestHeaders({
      headers: {
        cookie: "synara_login=session-token",
        connection: "keep-alive",
        "x-forwarded-for": "203.0.113.10",
        "x-forwarded-host": "spoofed.example",
        "x-forwarded-proto": "http",
      },
      requestUrl: new URL("https://synara.example/v1/auth/session"),
      remoteAddress: "127.0.0.1",
    });

    expect(headers.get("cookie")).toBe("synara_login=session-token");
    expect(headers.get("connection")).toBeNull();
    expect(headers.get("x-forwarded-for")).toBe("127.0.0.1");
    expect(headers.get("x-forwarded-host")).toBe("synara.example");
    expect(headers.get("x-forwarded-proto")).toBe("https");
  });

  it("does not retain a spoofed forwarded address when the socket address is unavailable", () => {
    const headers = buildControlPlaneProxyRequestHeaders({
      headers: { "x-forwarded-for": "203.0.113.10" },
      requestUrl: new URL("https://synara.example/v1/platform/profile"),
    });

    expect(headers.get("x-forwarded-for")).toBeNull();
  });

  it("allows the installed Desktop origin to read and mutate through the local proxy", () => {
    expect(
      buildControlPlaneProxyCorsHeaders({
        rawOrigin: "synara://app",
        requestOrigin: "http://127.0.0.1:58180",
        config: localDesktopConfig,
      }),
    ).toMatchObject({
      "Access-Control-Allow-Origin": "synara://app",
      "Access-Control-Allow-Credentials": "true",
      "Access-Control-Allow-Methods": expect.stringContaining("DELETE"),
      "Access-Control-Allow-Headers": expect.stringContaining("Idempotency-Key"),
    });
  });

  it("rejects an unrelated browser origin before forwarding Desktop credentials", () => {
    expect(
      buildControlPlaneProxyCorsHeaders({
        rawOrigin: "https://attacker.example",
        requestOrigin: "http://127.0.0.1:58180",
        config: localDesktopConfig,
      }),
    ).toBeNull();
  });

  it("keeps non-browser proxy requests available without adding CORS headers", () => {
    expect(
      buildControlPlaneProxyCorsHeaders({
        rawOrigin: undefined,
        requestOrigin: "http://127.0.0.1:58180",
        config: localDesktopConfig,
      }),
    ).toEqual({});
  });

  it("preserves multiple login cookies from the Control Plane", () => {
    const upstreamHeaders = new Headers({ "Content-Type": "application/json" });
    upstreamHeaders.append("Set-Cookie", "session=one; Path=/; HttpOnly");
    upstreamHeaders.append("Set-Cookie", "csrf=two; Path=/; SameSite=Lax");
    const headers = buildControlPlaneProxyResponseHeaders(
      new Response("{}", { headers: upstreamHeaders }),
    );

    expect(headers["set-cookie"]).toEqual([
      "session=one; Path=/; HttpOnly",
      "csrf=two; Path=/; SameSite=Lax",
    ]);
    expect(headers["content-type"]).toBe("application/json");
  });

  it("overrides upstream CORS policy while preserving its Vary dimensions", () => {
    const headers = buildControlPlaneProxyResponseHeaders(
      new Response("{}", { headers: { Vary: "Accept-Encoding" } }),
      {
        "Access-Control-Allow-Origin": "synara://app",
        Vary: "Origin",
      },
    );

    expect(headers["access-control-allow-origin"]).toBe("synara://app");
    expect(headers.vary).toBe("Accept-Encoding, Origin");
  });
});
