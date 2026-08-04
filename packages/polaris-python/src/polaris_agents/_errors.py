from __future__ import annotations

from typing import Any


class PolarisError(Exception):
    def __init__(
        self,
        status: int,
        code: str,
        message: str,
        request_id: str | None = None,
        details: dict[str, Any] | None = None,
        retry_after_seconds: int | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.request_id = request_id
        self.details = details
        self.retry_after_seconds = retry_after_seconds


class PolarisTransportError(Exception):
    pass


class PolarisSequenceGapError(Exception):
    def __init__(self, expected: int, received: int) -> None:
        super().__init__(f"Polaris Event sequence gap: expected {expected}, received {received}.")
        self.expected = expected
        self.received = received
