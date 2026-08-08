import { PolarisError, PolarisSequenceGapError, PolarisTransportError } from "./errors";
import { delay, PolarisTransport, responseError } from "./transport";
import type { SessionEvent } from "./types";

export type SessionEventStreamOptions = {
  afterSequence?: number;
  signal?: AbortSignal;
  reconnect?: boolean;
  reconnectDelayMs?: number;
  fallbackToPolling?: boolean;
  pollingIntervalMs?: number;
  pollingLimit?: number;
};

export async function* streamSessionEvents(
  transport: PolarisTransport,
  sessionId: string,
  options: SessionEventStreamOptions = {},
): AsyncGenerator<SessionEvent, void, void> {
  let cursor = normalizeSequence(options.afterSequence);
  const reconnect = options.reconnect ?? true;
  const reconnectDelayMs = normalizeReconnectDelay(options.reconnectDelayMs);
  let openingFailures = 0;

  while (!options.signal?.aborted) {
    const headers = transport.authorizationHeaders();
    headers.set("Accept", "text/event-stream");
    const query = new URLSearchParams({ afterSequence: String(cursor) });
    const requestAbort = new AbortController();
    const requestSignal = options.signal
      ? AbortSignal.any([options.signal, requestAbort.signal])
      : requestAbort.signal;
    let response: Response;
    try {
      response = await transport.fetch(
        transport.url(
          `/v1/sessions/${encodeURIComponent(sessionId)}/events/stream?${query.toString()}`,
        ),
        { headers, signal: requestSignal },
      );
    } catch (error) {
      requestAbort.abort();
      if (options.signal?.aborted) throw error;
      if (openingFailures >= transport.maxRetries) {
        throw new PolarisTransportError(
          "Polaris Event stream failed before a response was received.",
          { cause: error },
        );
      }
      openingFailures += 1;
      await delay(reconnectDelayMs, options.signal);
      continue;
    }
    if (!response.ok) {
      const error = await responseError(response);
      requestAbort.abort();
      if (isSSEConnectionLimit(error) && (options.fallbackToPolling ?? true)) {
        yield* pollSessionEvents(transport, sessionId, cursor, options);
        return;
      }
      if (!isRetryableStatus(response.status) || openingFailures >= transport.maxRetries)
        throw error;
      openingFailures += 1;
      await delay((error.retryAfterSeconds ?? reconnectDelayMs / 1000) * 1000, options.signal);
      continue;
    }
    openingFailures = 0;
    if (!response.body) {
      requestAbort.abort();
      throw new PolarisTransportError("Polaris Event stream response has no readable body.");
    }

    try {
      for await (const message of decodeEventStream(response.body, requestSignal)) {
        if (message.event !== "session-event" || message.data === "") continue;
        const event = parseSessionEvent(message.data);
        if (event.sequence <= cursor) continue;
        if (event.sequence !== cursor + 1) {
          throw new PolarisSequenceGapError(cursor + 1, event.sequence);
        }
        cursor = event.sequence;
        yield event;
      }
    } finally {
      requestAbort.abort();
    }
    if (!reconnect || options.signal?.aborted) return;
    await delay(reconnectDelayMs, options.signal);
  }
}

async function* pollSessionEvents(
  transport: PolarisTransport,
  sessionId: string,
  initialCursor: number,
  options: SessionEventStreamOptions,
): AsyncGenerator<SessionEvent, void, void> {
  let cursor = initialCursor;
  const interval = normalizePollingInterval(options.pollingIntervalMs);
  const limit = normalizePollingLimit(options.pollingLimit);
  while (!options.signal?.aborted) {
    const query = new URLSearchParams({ afterSequence: String(cursor), limit: String(limit) });
    const response = await transport.json<unknown>({
      method: "GET",
      path: `/v1/sessions/${encodeURIComponent(sessionId)}/events?${query.toString()}`,
      signal: options.signal,
    });
    const page = parseSessionEventPage(response.data);
    for (const event of page.items) {
      if (event.sequence <= cursor) continue;
      if (event.sequence !== cursor + 1) {
        throw new PolarisSequenceGapError(cursor + 1, event.sequence);
      }
      cursor = event.sequence;
      yield event;
    }
    if (page.items.length >= limit) continue;
    await delay(interval, options.signal);
  }
}

type SSEMessage = { event: string; data: string };

async function* decodeEventStream(
  body: ReadableStream<Uint8Array>,
  signal?: AbortSignal,
): AsyncGenerator<SSEMessage, void, void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (!signal?.aborted) {
      const { done, value } = await reader.read();
      buffer += decoder.decode(value, { stream: !done }).replaceAll("\r\n", "\n");
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        const frame = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const message = parseFrame(frame);
        if (message) yield message;
        boundary = buffer.indexOf("\n\n");
      }
      if (done) return;
    }
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}

function parseFrame(frame: string): SSEMessage | null {
  let event = "message";
  const data: string[] = [];
  for (const line of frame.split("\n")) {
    if (line === "" || line.startsWith(":")) continue;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    if (field === "data") data.push(value);
  }
  if (data.length === 0) return null;
  return { event, data: data.join("\n") };
}

function parseSessionEvent(data: string): SessionEvent {
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch (error) {
    throw new PolarisTransportError("Polaris Event stream contained invalid JSON.", {
      cause: error,
    });
  }
  return validateSessionEvent(value);
}

function validateSessionEvent(value: unknown): SessionEvent {
  const sequence = isRecord(value) ? value.sequence : undefined;
  if (typeof sequence !== "number" || !Number.isSafeInteger(sequence) || sequence < 1) {
    throw new PolarisTransportError("Polaris Event stream contained an invalid Session Event.");
  }
  return value as SessionEvent;
}

function parseSessionEventPage(value: unknown): { items: SessionEvent[]; lastSequence: number } {
  if (
    !isRecord(value) ||
    !Array.isArray(value.items) ||
    typeof value.lastSequence !== "number" ||
    !Number.isSafeInteger(value.lastSequence) ||
    value.lastSequence < 0
  ) {
    throw new PolarisTransportError("Polaris Event polling returned an invalid page.");
  }
  return { items: value.items.map(validateSessionEvent), lastSequence: value.lastSequence };
}

function normalizeSequence(value: number | undefined): number {
  if (value === undefined) return 0;
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError("afterSequence must be a non-negative safe integer.");
  }
  return value;
}

function normalizeReconnectDelay(value: number | undefined): number {
  if (value === undefined) return 500;
  if (!Number.isSafeInteger(value) || value < 0 || value > 60_000) {
    throw new TypeError("reconnectDelayMs must be an integer between 0 and 60000.");
  }
  return value;
}

function normalizePollingInterval(value: number | undefined): number {
  if (value === undefined) return 1_000;
  if (!Number.isSafeInteger(value) || value < 0 || value > 60_000) {
    throw new TypeError("pollingIntervalMs must be an integer between 0 and 60000.");
  }
  return value;
}

function normalizePollingLimit(value: number | undefined): number {
  if (value === undefined) return 50;
  if (!Number.isSafeInteger(value) || value < 1 || value > 200) {
    throw new TypeError("pollingLimit must be an integer between 1 and 200.");
  }
  return value;
}

function isSSEConnectionLimit(error: PolarisError): boolean {
  return (
    error.status === 429 &&
    (error.code === "sse_user_connection_limit" || error.code === "sse_tenant_connection_limit")
  );
}

function isRetryableStatus(status: number): boolean {
  return status === 429 || status === 500 || status === 502 || status === 503 || status === 504;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
