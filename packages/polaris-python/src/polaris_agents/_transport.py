from __future__ import annotations

import json
import time
from dataclasses import dataclass
from typing import Any, Callable, Generic, Mapping, TypeVar, cast
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import Request, urlopen

from ._errors import PolarisError, PolarisTransportError

T = TypeVar("T")
OpenURL = Callable[..., Any]


@dataclass(frozen=True)
class RateLimit:
    limit: int | None
    remaining: int | None
    reset_after_seconds: int | None


@dataclass(frozen=True)
class ResponseMetadata:
    request_id: str | None
    idempotency_replayed: bool
    rate_limit: RateLimit


@dataclass(frozen=True)
class SDKResponse(Generic[T]):
    data: T
    metadata: ResponseMetadata

    @property
    def request_id(self) -> str | None:
        return self.metadata.request_id

    @property
    def idempotency_replayed(self) -> bool:
        return self.metadata.idempotency_replayed

    @property
    def rate_limit(self) -> RateLimit:
        return self.metadata.rate_limit


class PolarisTransport:
    def __init__(
        self,
        *,
        api_key: str,
        base_url: str,
        max_retries: int = 2,
        timeout: float = 30.0,
        opener: OpenURL = urlopen,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        key = api_key.strip()
        if not key.startswith("syna_sa_") or len(key) <= len("syna_sa_"):
            raise ValueError("Polaris api_key must be a syna_sa_ Service Account token.")
        parsed = urlsplit(base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc or parsed.username or parsed.password:
            raise ValueError("Polaris base_url must be an HTTP(S) URL without embedded credentials.")
        if parsed.query or parsed.fragment:
            raise ValueError("Polaris base_url must not contain a query or fragment.")
        if not isinstance(max_retries, int) or isinstance(max_retries, bool) or not 0 <= max_retries <= 10:
            raise ValueError("Polaris max_retries must be an integer between 0 and 10.")
        if timeout <= 0 or timeout > 300:
            raise ValueError("Polaris timeout must be greater than 0 and at most 300 seconds.")
        self.api_key = key
        self.base_url = base_url.rstrip("/")
        self.max_retries = max_retries
        self.timeout = timeout
        self._opener = opener
        self._sleep = sleep

    def request_json(
        self,
        method: str,
        path: str,
        *,
        body: Mapping[str, Any] | None = None,
        query: Mapping[str, str | int] | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[dict[str, Any]]:
        url = self._url(path, query)
        encoded = json.dumps(body, separators=(",", ":")).encode() if body is not None else None
        retryable = method == "GET" or bool(idempotency_key)
        last_error: BaseException | None = None
        for attempt in range(self.max_retries + 1):
            request = Request(url, data=encoded, method=method, headers=self._headers(idempotency_key))
            try:
                response = self._opener(request, timeout=self.timeout)
                try:
                    payload = {} if response.getcode() == 204 else json.loads(response.read())
                    if not isinstance(payload, dict):
                        raise PolarisTransportError("Polaris returned a non-object JSON response.")
                    return SDKResponse(cast(dict[str, Any], payload), _metadata(response.headers))
                finally:
                    response.close()
            except HTTPError as error:
                parsed = _http_error(error)
                if not retryable or error.code not in {429, 500, 502, 503, 504} or attempt == self.max_retries:
                    raise parsed from error
                self._sleep(float(parsed.retry_after_seconds or _backoff(attempt)))
            except (URLError, OSError, TimeoutError) as error:
                last_error = error
                if not retryable or attempt == self.max_retries:
                    break
                self._sleep(float(_backoff(attempt)))
        raise PolarisTransportError("Polaris request failed before a response was received.") from last_error

    def open_event_stream(self, path: str, *, after_sequence: int) -> Any:
        request = Request(
            self._url(path, {"afterSequence": after_sequence}),
            method="GET",
            headers={"Authorization": f"Bearer {self.api_key}", "Accept": "text/event-stream"},
        )
        try:
            return self._opener(request, timeout=self.timeout)
        except HTTPError as error:
            raise _http_error(error) from error
        except (URLError, OSError, TimeoutError) as error:
            raise PolarisTransportError("Polaris Event stream failed before a response was received.") from error

    def _headers(self, idempotency_key: str | None) -> dict[str, str]:
        headers = {"Authorization": f"Bearer {self.api_key}", "Accept": "application/json"}
        if idempotency_key is not None:
            headers.update({"Content-Type": "application/json", "Idempotency-Key": idempotency_key})
        return headers

    def _url(self, path: str, query: Mapping[str, str | int] | None = None) -> str:
        suffix = f"?{urlencode(query)}" if query else ""
        return f"{self.base_url}{path}{suffix}"


def _metadata(headers: Any) -> ResponseMetadata:
    return ResponseMetadata(
        request_id=headers.get("X-Request-ID"),
        idempotency_replayed=headers.get("Idempotency-Replayed") == "true",
        rate_limit=RateLimit(
            limit=_integer_header(headers, "RateLimit-Limit", positive=True),
            remaining=_integer_header(headers, "RateLimit-Remaining", positive=False),
            reset_after_seconds=_integer_header(headers, "RateLimit-Reset", positive=True),
        ),
    )


def _http_error(error: HTTPError) -> PolarisError:
    try:
        envelope = json.loads(error.read())
    except (json.JSONDecodeError, UnicodeDecodeError):
        envelope = None
    value = envelope.get("error") if isinstance(envelope, dict) else None
    value = value if isinstance(value, dict) else {}
    details = value.get("details")
    return PolarisError(
        error.code,
        value.get("code") if isinstance(value.get("code"), str) else "unexpected_response",
        value.get("message") if isinstance(value.get("message"), str) else f"Polaris returned HTTP {error.code}.",
        value.get("requestId") if isinstance(value.get("requestId"), str) else error.headers.get("X-Request-ID"),
        details if isinstance(details, dict) else None,
        _integer_header(error.headers, "Retry-After", positive=True),
    )


def _integer_header(headers: Any, name: str, *, positive: bool) -> int | None:
    raw = headers.get(name)
    if not isinstance(raw, str) or not raw.isascii() or not raw.isdecimal():
        return None
    value = int(raw)
    if value < (1 if positive else 0):
        return None
    return value


def _backoff(attempt: int) -> float:
    return min(0.25 * (2**attempt), 4.0)
