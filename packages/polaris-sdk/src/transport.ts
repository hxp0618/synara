import { PolarisError, PolarisTransportError } from "./errors";
import type { RateLimit, SDKResponse } from "./types";

type Fetch = typeof globalThis.fetch;

export type TransportOptions = {
  apiKey: string;
  baseUrl: string;
  fetch?: Fetch;
  maxRetries?: number;
};

type JSONRequest =
  | { method: "GET"; path: string; signal: AbortSignal | undefined }
  | {
      method: "POST" | "PATCH" | "DELETE";
      path: string;
      body?: unknown;
      idempotencyKey?: string;
      signal: AbortSignal | undefined;
    };

export class PolarisTransport {
  readonly apiKey: string;
  readonly baseUrl: string;
  readonly fetch: Fetch;
  readonly maxRetries: number;

  constructor(options: TransportOptions) {
    this.apiKey = validateAPIKey(options.apiKey);
    this.baseUrl = validateBaseURL(options.baseUrl);
    this.fetch = options.fetch ?? globalThis.fetch;
    if (typeof this.fetch !== "function") {
      throw new TypeError("Polaris requires a Fetch API implementation.");
    }
    this.maxRetries = normalizeMaxRetries(options.maxRetries);
  }

  url(path: string): string {
    return `${this.baseUrl}${path}`;
  }

  authorizationHeaders(): Headers {
    return new Headers({ Authorization: `Bearer ${this.apiKey}` });
  }

  async json<T>(request: JSONRequest): Promise<SDKResponse<T>> {
    let lastNetworkError: unknown;
    for (let attempt = 0; attempt <= this.maxRetries; attempt += 1) {
      try {
        const headers = this.authorizationHeaders();
        headers.set("Accept", "application/json");
        if ("body" in request && request.body !== undefined)
          headers.set("Content-Type", "application/json");
        if ("idempotencyKey" in request && request.idempotencyKey !== undefined)
          headers.set("Idempotency-Key", request.idempotencyKey);
        const response = await this.fetch(this.url(request.path), {
          method: request.method,
          headers,
          body:
            "body" in request && request.body !== undefined ? JSON.stringify(request.body) : null,
          signal: request.signal ?? null,
        });
        if (response.ok) {
          return {
            data: (response.status === 204 ? null : await response.json()) as T,
            ...responseMetadata(response),
          };
        }
        const error = await responseError(response);
        if (
          !isRetryableRequest(request) ||
          !isRetryableStatus(response.status) ||
          attempt === this.maxRetries
        ) {
          throw error;
        }
        await waitBeforeRetry(response, attempt, request.signal);
      } catch (error) {
        if (error instanceof PolarisError) throw error;
        if (request.signal?.aborted) throw error;
        lastNetworkError = error;
        if (!isRetryableRequest(request) || attempt === this.maxRetries) break;
        await delay(exponentialDelay(attempt), request.signal);
      }
    }
    throw new PolarisTransportError("Polaris request failed before a response was received.", {
      cause: lastNetworkError,
    });
  }
}

function isRetryableRequest(request: JSONRequest): boolean {
  return (
    request.method === "GET" ||
    ("idempotencyKey" in request && (request.idempotencyKey?.length ?? 0) > 0)
  );
}

export function responseMetadata(response: Response) {
  return {
    requestId: response.headers.get("X-Request-ID"),
    idempotencyReplayed: response.headers.get("Idempotency-Replayed") === "true",
    rateLimit: rateLimitFromHeaders(response.headers),
  };
}

export async function responseError(response: Response): Promise<PolarisError> {
  let envelope: unknown;
  try {
    envelope = await response.json();
  } catch {
    envelope = null;
  }
  const error = isRecord(envelope) && isRecord(envelope.error) ? envelope.error : null;
  const code = typeof error?.code === "string" ? error.code : "unexpected_response";
  const message =
    typeof error?.message === "string"
      ? error.message
      : `Polaris returned HTTP ${response.status}.`;
  const requestId =
    typeof error?.requestId === "string" ? error.requestId : response.headers.get("X-Request-ID");
  const details = isRecord(error?.details) ? error.details : null;
  return new PolarisError(
    response.status,
    code,
    message,
    requestId,
    details,
    positiveIntegerHeader(response.headers, "Retry-After"),
  );
}

function rateLimitFromHeaders(headers: Headers): RateLimit {
  return {
    limit: positiveIntegerHeader(headers, "RateLimit-Limit"),
    remaining: nonNegativeIntegerHeader(headers, "RateLimit-Remaining"),
    resetAfterSeconds: positiveIntegerHeader(headers, "RateLimit-Reset"),
  };
}

function positiveIntegerHeader(headers: Headers, name: string): number | null {
  const value = nonNegativeIntegerHeader(headers, name);
  return value !== null && value > 0 ? value : null;
}

function nonNegativeIntegerHeader(headers: Headers, name: string): number | null {
  const raw = headers.get(name);
  if (raw === null || !/^[0-9]+$/.test(raw)) return null;
  const value = Number(raw);
  return Number.isSafeInteger(value) ? value : null;
}

function validateAPIKey(value: string): string {
  const apiKey = value.trim();
  if (!apiKey.startsWith("syna_sa_") || apiKey.length <= "syna_sa_".length) {
    throw new TypeError("Polaris apiKey must be a syna_sa_ Service Account token.");
  }
  return apiKey;
}

function validateBaseURL(value: string): string {
  const parsed = new URL(value);
  if (
    (parsed.protocol !== "https:" && parsed.protocol !== "http:") ||
    parsed.username ||
    parsed.password
  ) {
    throw new TypeError("Polaris baseUrl must be an HTTP(S) URL without embedded credentials.");
  }
  if (parsed.search || parsed.hash) {
    throw new TypeError("Polaris baseUrl must not contain a query or fragment.");
  }
  return parsed.toString().replace(/\/+$/, "");
}

function normalizeMaxRetries(value: number | undefined): number {
  if (value === undefined) return 2;
  if (!Number.isSafeInteger(value) || value < 0 || value > 10) {
    throw new TypeError("Polaris maxRetries must be an integer between 0 and 10.");
  }
  return value;
}

function isRetryableStatus(status: number): boolean {
  return status === 429 || status === 500 || status === 502 || status === 503 || status === 504;
}

async function waitBeforeRetry(response: Response, attempt: number, signal?: AbortSignal) {
  const retryAfter = positiveIntegerHeader(response.headers, "Retry-After");
  await delay(retryAfter === null ? exponentialDelay(attempt) : retryAfter * 1000, signal);
}

function exponentialDelay(attempt: number): number {
  return Math.min(250 * 2 ** attempt, 4000);
}

export function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(signal.reason);
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(resolve, milliseconds);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timeout);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
