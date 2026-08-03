// FILE: controlPlaneProxy.ts
// Purpose: Proxy same-origin /v1 Control Plane API requests to the optional Go control plane.
// Layer: Server HTTP transport

import { NodeHttpServerRequest } from "@effect/platform-node";
import { Effect, Layer, Stream } from "effect";
import { HttpRouter, HttpServerRequest, HttpServerResponse } from "effect/unstable/http";

import { ServerConfig, type ServerConfigShape } from "./config";
import { isTrustedAppOrigin, normalizeCorsOrigin } from "./trustedOrigins";

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

const CONTROL_PLANE_PROXY_CORS_METHODS = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS";
const CONTROL_PLANE_PROXY_CORS_HEADERS =
  "Authorization, Content-Type, Idempotency-Key, Last-Event-ID";

function unavailableResponse(
  status: 403 | 502 | 503,
  code: string,
  message: string,
  headers: Readonly<Record<string, string>> = {},
) {
  return HttpServerResponse.jsonUnsafe(
    { error: { code, message, details: null } },
    { status, headers: { "Cache-Control": "no-store", ...headers } },
  );
}

export function buildControlPlaneProxyCorsHeaders(input: {
  readonly rawOrigin: string | ReadonlyArray<string> | undefined;
  readonly requestOrigin: string;
  readonly config: ServerConfigShape;
}): Record<string, string> | null {
  if (input.rawOrigin === undefined) return {};
  const origin = normalizeCorsOrigin(input.rawOrigin);
  if (
    !origin ||
    !isTrustedAppOrigin({ origin, requestOrigin: input.requestOrigin, config: input.config })
  ) {
    return null;
  }
  return {
    "Access-Control-Allow-Origin": origin,
    "Access-Control-Allow-Credentials": "true",
    "Access-Control-Allow-Methods": CONTROL_PLANE_PROXY_CORS_METHODS,
    "Access-Control-Allow-Headers": CONTROL_PLANE_PROXY_CORS_HEADERS,
    "Access-Control-Expose-Headers": "Content-Disposition, Location",
    Vary: "Origin",
  };
}

function mergeVaryHeader(
  current: string | ReadonlyArray<string> | undefined,
  addition: string,
): string {
  const values = [
    ...(Array.isArray(current) ? current : current ? [current] : []),
    addition,
  ].flatMap((value) => value.split(","));
  const unique = new Map<string, string>();
  for (const value of values) {
    const normalized = value.trim();
    if (normalized) unique.set(normalized.toLowerCase(), normalized);
  }
  return [...unique.values()].join(", ");
}

export function resolveControlPlaneTarget(baseUrl: URL, requestUrl: URL): URL {
  const target = new URL(baseUrl);
  const basePath = target.pathname.replace(/\/+$/u, "");
  target.pathname = `${basePath}${requestUrl.pathname.startsWith("/") ? requestUrl.pathname : `/${requestUrl.pathname}`}`;
  target.search = requestUrl.search;
  target.hash = "";
  return target;
}

export function shouldStreamControlPlaneResponse(response: Response): boolean {
  if (response.body === null) return false;
  if (response.headers.get("content-type")?.startsWith("text/event-stream") === true) return true;
  return (
    response.headers.get("content-disposition")?.toLowerCase().startsWith("attachment;") === true
  );
}

export function streamControlPlaneResponseBody(
  body: ReadableStream<Uint8Array>,
  finalizeUpstream: () => void,
) {
  return Stream.fromReadableStream({
    evaluate: () => body,
    onError: (cause) => cause,
  }).pipe(Stream.ensuring(Effect.sync(finalizeUpstream)));
}

export function bindControlPlaneProxyAbort(
  downstream: {
    request: {
      readonly complete: boolean;
      once(event: "aborted" | "close", listener: () => void): unknown;
      off(event: "aborted" | "close", listener: () => void): unknown;
    };
    response: {
      readonly writableEnded: boolean;
      once(event: "close", listener: () => void): unknown;
      off(event: "close", listener: () => void): unknown;
    };
  },
  upstreamAbort: AbortController,
) {
  const abortUpstream = () => upstreamAbort.abort();
  const abortIncompleteRequest = () => {
    if (!downstream.request.complete) abortUpstream();
  };
  const abortIncompleteResponse = () => {
    if (!downstream.response.writableEnded) abortUpstream();
  };
  downstream.request.once("aborted", abortUpstream);
  downstream.request.once("close", abortIncompleteRequest);
  downstream.response.once("close", abortIncompleteResponse);
  return () => {
    downstream.request.off("aborted", abortUpstream);
    downstream.request.off("close", abortIncompleteRequest);
    downstream.response.off("close", abortIncompleteResponse);
    upstreamAbort.abort();
  };
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

export function buildControlPlaneProxyResponseHeaders(
  response: Response,
  corsHeaders: Readonly<Record<string, string>> = {},
) {
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
  for (const [name, value] of Object.entries(corsHeaders)) {
    const normalizedName = name.toLowerCase();
    if (normalizedName === "vary") {
      headers.vary = mergeVaryHeader(headers.vary, value);
    } else {
      headers[normalizedName] = value;
    }
  }
  return headers;
}

const proxyControlPlaneRequest = Effect.gen(function* () {
  const config = yield* ServerConfig;
  const request = yield* HttpServerRequest.HttpServerRequest;
  const requestUrl = HttpServerRequest.toURL(request);
  if (!requestUrl) {
    return unavailableResponse(
      502,
      "invalid_proxy_request",
      "The control-plane request URL is invalid.",
    );
  }
  const corsHeaders = buildControlPlaneProxyCorsHeaders({
    rawOrigin: request.headers.origin,
    requestOrigin: requestUrl.origin,
    config,
  });
  if (corsHeaders === null) {
    return unavailableResponse(
      403,
      "untrusted_proxy_origin",
      "A trusted application origin is required for Control Plane proxy requests.",
    );
  }
  if (request.method === "OPTIONS") {
    return HttpServerResponse.empty({ status: 204, headers: corsHeaders });
  }
  if (!config.controlPlaneUrl) {
    return unavailableResponse(
      503,
      "control_plane_unavailable",
      "The Control Plane is not configured for this Synara instance.",
      corsHeaders,
    );
  }

  const webRequest = yield* HttpServerRequest.toWeb(request);
  const body = request.method === "GET" || request.method === "HEAD" ? undefined : webRequest.body;
  const upstreamAbort = new AbortController();
  const finalizeUpstream = bindControlPlaneProxyAbort(
    {
      request: NodeHttpServerRequest.toIncomingMessage(request),
      response: NodeHttpServerRequest.toServerResponse(request),
    },
    upstreamAbort,
  );
  const response = yield* Effect.tryPromise({
    try: (signal) => {
      const abortUpstream = () => upstreamAbort.abort(signal.reason);
      signal.addEventListener("abort", abortUpstream, { once: true });
      return fetch(resolveControlPlaneTarget(config.controlPlaneUrl!, requestUrl), {
        method: request.method,
        headers: buildControlPlaneProxyRequestHeaders({
          headers: request.headers,
          requestUrl,
          ...(request.remoteAddress ? { remoteAddress: request.remoteAddress } : {}),
        }),
        body,
        ...(body === undefined ? {} : { duplex: "half" as const }),
        redirect: "manual",
        signal: upstreamAbort.signal,
      } as RequestInit & { duplex?: "half" }).finally(() => {
        signal.removeEventListener("abort", abortUpstream);
      });
    },
    catch: (cause) => cause,
  }).pipe(Effect.option);

  if (response._tag === "None") {
    finalizeUpstream();
    return unavailableResponse(
      502,
      "control_plane_proxy_failed",
      "The Control Plane could not be reached.",
      corsHeaders,
    );
  }
  const upstream = response.value;
  if (shouldStreamControlPlaneResponse(upstream)) {
    return HttpServerResponse.stream(
      streamControlPlaneResponseBody(upstream.body!, finalizeUpstream),
      {
        status: upstream.status,
        statusText: upstream.statusText,
        headers: buildControlPlaneProxyResponseHeaders(upstream, corsHeaders),
      },
    );
  }
  const bytes = new Uint8Array(
    yield* Effect.promise(() => upstream.arrayBuffer()).pipe(
      Effect.ensuring(Effect.sync(finalizeUpstream)),
    ),
  );
  return HttpServerResponse.uint8Array(bytes, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: buildControlPlaneProxyResponseHeaders(upstream, corsHeaders),
  });
});

export const controlPlaneProxyEffectRouteLayer = Layer.mergeAll(
  HttpRouter.add("*", "/v1/*", proxyControlPlaneRequest),
  HttpRouter.add("*", "/scim/*", proxyControlPlaneRequest),
);
