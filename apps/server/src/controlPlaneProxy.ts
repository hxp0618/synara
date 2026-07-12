// FILE: controlPlaneProxy.ts
// Purpose: Proxy same-origin /v1 SaaS API requests to the optional Go control plane.
// Layer: Server HTTP transport

import { Effect, Layer, Stream } from "effect";
import { HttpRouter, HttpServerRequest, HttpServerResponse } from "effect/unstable/http";

import { ServerConfig } from "./config";

const HOP_BY_HOP_HEADERS = new Set([
  "connection",
  "content-length",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

const RESPONSE_HEADERS_TO_DROP = new Set([...HOP_BY_HOP_HEADERS, "content-encoding"]);

function unavailableResponse(status: 502 | 503, code: string, message: string) {
  return HttpServerResponse.jsonUnsafe(
    { error: { code, message, details: null } },
    { status, headers: { "Cache-Control": "no-store" } },
  );
}

export function resolveControlPlaneTarget(baseUrl: URL, requestUrl: URL): URL {
  const target = new URL(baseUrl);
  target.pathname = requestUrl.pathname;
  target.search = requestUrl.search;
  target.hash = "";
  return target;
}

export function shouldStreamControlPlaneResponse(response: Response): boolean {
  if (response.body === null) return false;
  if (response.headers.get("content-type")?.startsWith("text/event-stream") === true) return true;
  return response.headers.get("content-disposition")?.toLowerCase().startsWith("attachment;") === true;
}

export function buildControlPlaneProxyRequestHeaders(input: {
  headers: Readonly<Record<string, string | undefined>>;
  requestUrl: URL;
  remoteAddress?: string | undefined;
}) {
  const headers = new Headers();
  for (const [name, value] of Object.entries(input.headers)) {
    if (value !== undefined && !HOP_BY_HOP_HEADERS.has(name.toLowerCase())) {
      headers.set(name, value);
    }
  }
  headers.set("x-forwarded-host", input.requestUrl.host);
  headers.set("x-forwarded-proto", input.requestUrl.protocol.slice(0, -1));
  if (input.remoteAddress) {
    headers.set("x-forwarded-for", input.remoteAddress);
  } else {
    headers.delete("x-forwarded-for");
  }
  return headers;
}

export function buildControlPlaneProxyResponseHeaders(response: Response) {
  const headers: Record<string, string | ReadonlyArray<string>> = {};
  response.headers.forEach((value, name) => {
    if (!RESPONSE_HEADERS_TO_DROP.has(name.toLowerCase()) && name.toLowerCase() !== "set-cookie") {
      headers[name] = value;
    }
  });
  const setCookies = (
    response.headers as Headers & { getSetCookie?: () => ReadonlyArray<string> }
  ).getSetCookie?.();
  if (setCookies && setCookies.length > 0) {
    headers["set-cookie"] = setCookies;
  } else {
    const setCookie = response.headers.get("set-cookie");
    if (setCookie) headers["set-cookie"] = setCookie;
  }
  return headers;
}

const proxyControlPlaneRequest = Effect.gen(function* () {
    const config = yield* ServerConfig;
    const request = yield* HttpServerRequest.HttpServerRequest;
    const requestUrl = HttpServerRequest.toURL(request);
    if (!requestUrl) {
      return unavailableResponse(502, "invalid_proxy_request", "The control-plane request URL is invalid.");
    }
    if (!config.controlPlaneUrl) {
      return unavailableResponse(
        503,
        "control_plane_unavailable",
        "The SaaS control plane is not configured for this Synara instance.",
      );
    }

    const webRequest = yield* HttpServerRequest.toWeb(request);
    const body = request.method === "GET" || request.method === "HEAD" ? undefined : webRequest.body;
    const response = yield* Effect.tryPromise({
      try: (signal) =>
        fetch(resolveControlPlaneTarget(config.controlPlaneUrl!, requestUrl), {
          method: request.method,
          headers: buildControlPlaneProxyRequestHeaders({
            headers: request.headers,
            requestUrl,
            ...(request.remoteAddress ? { remoteAddress: request.remoteAddress } : {}),
          }),
          body,
          ...(body === undefined ? {} : { duplex: "half" as const }),
          redirect: "manual",
          signal,
        } as RequestInit & { duplex?: "half" }),
      catch: (cause) => cause,
    }).pipe(Effect.option);

    if (response._tag === "None") {
      return unavailableResponse(
        502,
        "control_plane_proxy_failed",
        "The SaaS control plane could not be reached.",
      );
    }
    const upstream = response.value;
    if (shouldStreamControlPlaneResponse(upstream)) {
      return HttpServerResponse.stream(
        Stream.fromAsyncIterable(upstream.body!, (cause) => cause),
        {
          status: upstream.status,
          statusText: upstream.statusText,
          headers: buildControlPlaneProxyResponseHeaders(upstream),
        },
      );
    }
    const bytes = new Uint8Array(yield* Effect.promise(() => upstream.arrayBuffer()));
    return HttpServerResponse.uint8Array(bytes, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: buildControlPlaneProxyResponseHeaders(upstream),
    });
});

export const controlPlaneProxyEffectRouteLayer = Layer.mergeAll(
  HttpRouter.add("*", "/v1/*", proxyControlPlaneRequest),
  HttpRouter.add("*", "/scim/*", proxyControlPlaneRequest),
);
