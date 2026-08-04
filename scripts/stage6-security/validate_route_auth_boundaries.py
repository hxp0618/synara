#!/usr/bin/env python3
"""Inventory Control Plane routes and fail closed on an unclassified outer authentication boundary."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from collections import Counter


ROUTE_RE = re.compile(
    r'^\s*(mux|routes)\.(Handle|HandleFunc|Internal|InternalFunc|PublicBeta|PublicBetaFunc|PublicGA|PublicGAFunc)\("([A-Z]+) ([^"]+)", (.+)\)$'
)

PUBLIC_ROUTES = {
    "GET /health",
    "GET /ready",
    "GET /metrics",
    "GET /v1/platform/profile",
    "POST /v1/auth/dev-login",
    "GET /v1/auth/sso/connections",
    "GET /v1/auth/sso/{connectionID}/start",
    "GET /v1/auth/sso/{connectionID}/metadata",
    "GET /v1/auth/sso/{connectionID}/callback",
    "POST /v1/auth/sso/{connectionID}/callback",
}
PLATFORM_SIGNED_ROUTES = {
    "PUT /v1/platform/routing-authority/execution-targets/{executionTargetID}/observations",
}
WORKER_REGISTRATION_ROUTES = {"POST /v1/workers/register"}
ARTIFACT_TOKEN_ROUTES = {
    "PUT /v1/artifact-content/{artifactID}",
    "GET /v1/artifact-content/{artifactID}",
}
DESKTOP_ENROLLMENT_TOKEN_ROUTES = {
    "POST /v1/desktop-enrollments/redeem": "server.redeemDesktopEnrollment",
}
INTERNAL_SELF_HOSTED_FORBIDDEN_ROUTES = {
    "POST /v1/billing/stripe/webhook",
    "GET /v1/tenants/{tenantID}/commercial-billing",
    "POST /v1/tenants/{tenantID}/commercial-billing/checkout",
    "POST /v1/tenants/{tenantID}/commercial-billing/portal",
    "GET /v1/platform/billing-exercises",
    "POST /v1/platform/billing-exercises",
    "POST /v1/platform/billing-exercises/{billingExerciseID}/approvals",
}
INTERNAL_SELF_HOSTED_FORBIDDEN_ROUTE_PATTERNS = (
    re.compile(r"/(?:commercial-billing|billing)(?:/|$)"),
    re.compile(r"/(?:checkout|payment|payments|stripe)(?:/|$)"),
)


class RouteBoundaryError(Exception):
    pass


def classify_route(route: str, registration: str, handler: str, exposure: str) -> str:
    identity = route
    if identity in PUBLIC_ROUTES:
        if exposure != "internal" or registration != "HandleFunc" or "server.require" in handler:
            raise RouteBoundaryError(f"public route {identity} has an unexpected wrapper")
        return "public"
    if identity in PLATFORM_SIGNED_ROUTES:
        if exposure != "internal" or registration != "HandleFunc" or handler != "server.publishPlatformRoutingAuthority":
            raise RouteBoundaryError(f"platform-signed route {identity} is not bound to its signature verifier")
        return "platform-signature"
    if identity in WORKER_REGISTRATION_ROUTES:
        if exposure != "internal" or registration != "HandleFunc" or handler != "server.registerWorker":
            raise RouteBoundaryError(f"Worker registration route {identity} is not bound to registration-token verification")
        return "worker-registration-token"
    if identity in ARTIFACT_TOKEN_ROUTES:
        expected = "server.uploadArtifactContent" if identity.startswith("PUT ") else "server.downloadArtifactContent"
        if exposure != "internal" or registration != "HandleFunc" or handler != expected:
            raise RouteBoundaryError(f"Artifact content route {identity} is not bound to its scoped content-token verifier")
        return "artifact-content-token"
    if identity in DESKTOP_ENROLLMENT_TOKEN_ROUTES:
        expected = DESKTOP_ENROLLMENT_TOKEN_ROUTES[identity]
        if exposure != "internal" or registration != "HandleFunc" or handler != expected:
            raise RouteBoundaryError(
                f"Desktop Enrollment route {identity} is not bound to its one-time handle verifier"
            )
        return "desktop-enrollment-token"
    _, path = identity.split(" ", 1)
    if path.startswith("/v1/workers/"):
        if exposure != "internal" or registration != "Handle" or not handler.startswith("server.requireWorker("):
            raise RouteBoundaryError(f"Worker route {identity} is missing requireWorker")
        return "worker"
    if path.startswith("/scim/v2/"):
        if exposure != "internal" or registration != "Handle" or not handler.startswith("server.requireServiceAccount("):
            raise RouteBoundaryError(f"SCIM route {identity} is missing requireServiceAccount")
        return "service-account"
    if path.startswith("/v1/"):
        if exposure in {"public-beta", "public-ga"}:
            if registration != "Handle" or not handler.startswith("server.requireDeveloperAuth("):
                raise RouteBoundaryError(f"developer route {identity} is missing requireDeveloperAuth")
            return "developer-auth"
        if exposure != "internal":
            raise RouteBoundaryError(f"authenticated route {identity} has an unsupported exposure")
        if registration != "Handle" or not handler.startswith("server.requireAuth("):
            raise RouteBoundaryError(f"authenticated route {identity} is missing requireAuth")
        return "login-session"
    raise RouteBoundaryError(f"route {identity} is outside every approved boundary")


def validate(source_path: pathlib.Path) -> dict[str, object]:
    try:
        source_bytes = source_path.read_bytes()
        source = source_bytes.decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise RouteBoundaryError("route source could not be read as UTF-8") from error
    routes: dict[str, str] = {}
    counts: Counter[str] = Counter()
    exposure_counts: Counter[str] = Counter()
    registration_lines = 0
    for line_number, line in enumerate(source.splitlines(), start=1):
        stripped = line.lstrip()
        if not (stripped.startswith("mux.Handle") or stripped.startswith("routes.")):
            continue
        registration_lines += 1
        match = ROUTE_RE.fullmatch(line)
        if match is None:
            raise RouteBoundaryError(f"route registration at line {line_number} is not statically classifiable")
        receiver, classified_registration, method, path, handler = match.groups()
        if receiver == "mux":
            exposure = "internal"
            registration = classified_registration
        else:
            exposure = (
                "public-beta"
                if classified_registration.startswith("PublicBeta")
                else "public-ga"
                if classified_registration.startswith("PublicGA")
                else "internal"
            )
            registration = "HandleFunc" if classified_registration.endswith("Func") else "Handle"
        identity = f"{method} {path}"
        if identity in routes:
            raise RouteBoundaryError(f"duplicate route registration {identity}")
        if identity in INTERNAL_SELF_HOSTED_FORBIDDEN_ROUTES or any(
            pattern.search(path) for pattern in INTERNAL_SELF_HOSTED_FORBIDDEN_ROUTE_PATTERNS
        ):
            raise RouteBoundaryError(
                f"payment route {identity} is forbidden in the internal-self-hosted product runtime"
            )
        boundary = classify_route(identity, registration, handler, exposure)
        routes[identity] = boundary
        counts[boundary] += 1
        exposure_counts[exposure] += 1
    if not routes or len(routes) != registration_lines:
        raise RouteBoundaryError("route inventory is empty or incomplete")
    tenant_routes = [identity for identity in routes if "/v1/tenants/{tenantID}" in identity]
    if not tenant_routes or any(
        routes[identity] not in {"login-session", "developer-auth"}
        for identity in tenant_routes
    ):
        raise RouteBoundaryError("one or more explicit Tenant routes are outside an approved authenticated boundary")
    return {
        "schemaVersion": "synara.route-auth-boundary-validation.v1",
        "source": {"path": source_path.name, "sha256": "sha256:" + hashlib.sha256(source_bytes).hexdigest()},
        "routes": {
            "total": len(routes),
            "explicitTenantRoutes": len(tenant_routes),
            "byBoundary": {key: counts[key] for key in sorted(counts)},
            "byExposure": {key: exposure_counts[key] for key in sorted(exposure_counts)},
        },
        "assessment": "route-auth-boundaries-validated-not-tenant-audit-passed",
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument("--source", default="services/control-plane/internal/httpapi/server.go")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    root = pathlib.Path(args.repository_root).resolve()
    source = pathlib.Path(args.source)
    if not source.is_absolute():
        source = root / source
    try:
        result = validate(source.resolve())
    except RouteBoundaryError as error:
        parser.exit(2, f"route boundary validation failed: {error}\n")
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
