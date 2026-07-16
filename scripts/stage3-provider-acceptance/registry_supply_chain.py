from __future__ import annotations

import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import secrets
import subprocess
import time
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


TOOLS_LOCK_PATH = pathlib.Path("deploy/worker/supply-chain-tools.lock")
SIGNING_POLICY_PATH = pathlib.Path("deploy/worker/signing-policy.json")
VULNERABILITY_POLICY_PATH = pathlib.Path("deploy/worker/vulnerability-policy.json")
REQUIRED_TOOLS = ("cosign", "trivy")
SUPPORTED_SIGNING_MODES = ("ephemeral-key", "keyless", "kms-key")
SUPPORTED_SEVERITIES = ("UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL")
TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS = 1.0
TRIVY_DATABASE_DOWNLOAD_RETRY_MARKERS = (
    "unexpected eof",
    "connection reset by peer",
    "i/o timeout",
    "tls handshake timeout",
    "temporary failure in name resolution",
    "502 bad gateway",
    "503 service unavailable",
)
COSIGN_CLAIM_TYPE = "https://sigstore.dev/cosign/sign/v1"
IMMUTABLE_IMAGE_PATTERN = re.compile(
    r"[A-Za-z0-9][A-Za-z0-9._:-]*(?:/[A-Za-z0-9][A-Za-z0-9._:-]*)+"
    r"@sha256:[0-9a-f]{64}"
)
VULNERABILITY_ID_PATTERN = re.compile(
    r"(?:CVE-[0-9]{4}-[0-9]{4,}|GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4})",
    re.IGNORECASE,
)
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")
KMS_KEY_REFERENCE_PATTERN = re.compile(
    r"(?:awskms|gcpkms|azurekms|hashivault)://[^\s@?#]{1,2048}"
)


@dataclasses.dataclass(frozen=True)
class ToolImages:
    cosign: str
    trivy: str
    lock_sha256: str

    def as_report(self) -> dict[str, str]:
        return {
            "cosign": self.cosign,
            "trivy": self.trivy,
            "lockSha256": self.lock_sha256,
        }


@dataclasses.dataclass(frozen=True)
class SigningPolicy:
    mode: str
    require_transparency_log: bool
    key_reference: str | None
    credential_environment: tuple[str, ...]
    identity_token_environment: str | None
    certificate_identity: str | None
    certificate_identity_regexp: str | None
    certificate_oidc_issuer: str | None
    certificate_oidc_issuer_regexp: str | None
    sha256: str

    @property
    def production_policy(self) -> bool:
        return self.mode in {"keyless", "kms-key"}

    def as_report(self) -> dict[str, Any]:
        report: dict[str, Any] = {
            "path": str(SIGNING_POLICY_PATH),
            "sha256": self.sha256,
            "mode": self.mode,
            "requireTransparencyLog": self.require_transparency_log,
            "productionPolicy": self.production_policy,
        }
        if self.key_reference is not None:
            report["keyReference"] = self.key_reference
            report["credentialEnvironmentCount"] = len(self.credential_environment)
        if self.certificate_identity is not None:
            report["certificateIdentity"] = self.certificate_identity
        if self.certificate_identity_regexp is not None:
            report["certificateIdentityRegexp"] = self.certificate_identity_regexp
        if self.certificate_oidc_issuer is not None:
            report["certificateOidcIssuer"] = self.certificate_oidc_issuer
        if self.certificate_oidc_issuer_regexp is not None:
            report["certificateOidcIssuerRegexp"] = self.certificate_oidc_issuer_regexp
        return report


@dataclasses.dataclass(frozen=True)
class VulnerabilityException:
    vulnerability_id: str
    package: str
    platform: str
    expires_at: dt.datetime
    owner: str
    reason: str

    @property
    def identity(self) -> tuple[str, str, str]:
        return (self.vulnerability_id, self.package, self.platform)


@dataclasses.dataclass(frozen=True)
class VulnerabilityPolicy:
    blocked_severities: tuple[str, ...]
    ignore_unfixed: bool
    fail_on_end_of_life_os: bool
    maximum_database_age_hours: int
    exceptions: tuple[VulnerabilityException, ...]
    sha256: str

    def as_report(self) -> dict[str, Any]:
        return {
            "path": str(VULNERABILITY_POLICY_PATH),
            "sha256": self.sha256,
            "blockedSeverities": list(self.blocked_severities),
            "ignoreUnfixed": self.ignore_unfixed,
            "failOnEndOfLifeOS": self.fail_on_end_of_life_os,
            "maximumDatabaseAgeHours": self.maximum_database_age_hours,
            "exceptionCount": len(self.exceptions),
        }


@dataclasses.dataclass(frozen=True)
class SupplyChainConfiguration:
    tools: ToolImages
    signing_policy: SigningPolicy
    vulnerability_policy: VulnerabilityPolicy

    def source_evidence(self) -> dict[str, Any]:
        return {
            "tools": self.tools.as_report(),
            "signingPolicy": self.signing_policy.as_report(),
            "vulnerabilityPolicy": self.vulnerability_policy.as_report(),
        }


@dataclasses.dataclass(frozen=True)
class SupplyChainOptions:
    repo_root: pathlib.Path
    state_dir: pathlib.Path
    image_repository: str
    docker_bin: str
    timeout_seconds: float
    insecure_registry: bool


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _parse_timestamp(value: Any, *, field: str) -> dt.datetime:
    if not isinstance(value, str) or not value.strip():
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy contained an invalid timestamp.",
            {"field": field},
        )
    normalized = value.strip().replace("Z", "+00:00")
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy contained an invalid timestamp.",
            {"field": field},
        ) from None
    if parsed.tzinfo is None:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy timestamps must include a timezone.",
            {"field": field},
        )
    return parsed.astimezone(dt.timezone.utc)


def _read_bytes(repo_root: pathlib.Path, relative_path: pathlib.Path, *, label: str) -> bytes:
    try:
        return (repo_root / relative_path).read_bytes()
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            f"Worker Registry gate could not read the checked-in {label}.",
        ) from None


def load_tool_images(repo_root: pathlib.Path) -> ToolImages:
    raw = _read_bytes(repo_root, TOOLS_LOCK_PATH, label="supply-chain tool lock")
    values: dict[str, str] = {}
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool lock was not UTF-8.",
        ) from None
    for line_number, raw_line in enumerate(text.splitlines(), start=1):
        line = raw_line.strip()
        if not line:
            continue
        if line.count("=") != 1:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tool lock contained a malformed entry.",
                {"line": line_number},
            )
        name, reference = (part.strip() for part in line.split("=", 1))
        if name not in REQUIRED_TOOLS or name in values:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tool lock contained an unknown or duplicate tool.",
                {"line": line_number, "tool": name},
            )
        if (
            IMMUTABLE_IMAGE_PATTERN.fullmatch(reference) is None
            or any(character.isspace() or ord(character) < 32 for character in reference)
            or "://" in reference
        ):
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tools must use credential-free digest-pinned image references.",
                {"line": line_number, "tool": name},
            )
        values[name] = reference
    if set(values) != set(REQUIRED_TOOLS):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool lock did not contain every required tool.",
            {"requiredTools": list(REQUIRED_TOOLS), "foundTools": sorted(values)},
        )
    return ToolImages(
        cosign=values["cosign"],
        trivy=values["trivy"],
        lock_sha256=_sha256(raw),
    )


def _optional_policy_text(value: Any, *, field: str, maximum_length: int = 2048) -> str | None:
    if value is None:
        return None
    if (
        not isinstance(value, str)
        or not value.strip()
        or len(value) > maximum_length
        or any(ord(character) < 32 for character in value)
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy contained an invalid text value.",
            {"field": field},
        )
    return value.strip()


def _validate_policy_regexp(value: str | None, *, field: str) -> None:
    if value is None:
        return
    if (
        not value.startswith("^")
        or not value.endswith("$")
        or re.search(r"\(\?(?:[=!]|<[=!]|P<)|\\[1-9]", value) is not None
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy used an unanchored or unsupported Cosign RE2 regexp.",
            {"field": field},
        )
    try:
        re.compile(value)
    except re.error:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy contained an invalid regexp.",
            {"field": field},
        ) from None


def _validate_exact_issuer(value: str | None) -> None:
    if value is None:
        return
    try:
        parsed = urllib.parse.urlsplit(value)
        hostname = parsed.hostname
    except ValueError:
        parsed = None
        hostname = None
    if (
        parsed is None
        or parsed.scheme != "https"
        or not hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry keyless OIDC issuer must be a credential-free HTTPS URL.",
            {"field": "certificateOidcIssuer"},
        )


def load_signing_policy(repo_root: pathlib.Path) -> SigningPolicy:
    raw = _read_bytes(repo_root, SIGNING_POLICY_PATH, label="signing policy")
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy was not valid JSON.",
        ) from None
    expected_keys = {
        "schemaVersion",
        "mode",
        "requireTransparencyLog",
        "keyReference",
        "credentialEnvironment",
        "identityTokenEnvironment",
        "certificateIdentity",
        "certificateIdentityRegexp",
        "certificateOidcIssuer",
        "certificateOidcIssuerRegexp",
    }
    if not isinstance(payload, dict) or set(payload) != expected_keys or payload.get("schemaVersion") != 1:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy schema was invalid.",
        )
    mode = payload.get("mode")
    require_tlog = payload.get("requireTransparencyLog")
    raw_environment = payload.get("credentialEnvironment")
    if (
        mode not in SUPPORTED_SIGNING_MODES
        or not isinstance(require_tlog, bool)
        or not isinstance(raw_environment, list)
        or not all(
            isinstance(name, str) and ENVIRONMENT_NAME_PATTERN.fullmatch(name) is not None
            for name in raw_environment
        )
        or len(set(raw_environment)) != len(raw_environment)
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy values were invalid.",
        )
    key_reference = _optional_policy_text(payload.get("keyReference"), field="keyReference")
    token_environment = _optional_policy_text(
        payload.get("identityTokenEnvironment"),
        field="identityTokenEnvironment",
        maximum_length=128,
    )
    certificate_identity = _optional_policy_text(
        payload.get("certificateIdentity"), field="certificateIdentity"
    )
    certificate_identity_regexp = _optional_policy_text(
        payload.get("certificateIdentityRegexp"), field="certificateIdentityRegexp"
    )
    certificate_issuer = _optional_policy_text(
        payload.get("certificateOidcIssuer"), field="certificateOidcIssuer"
    )
    certificate_issuer_regexp = _optional_policy_text(
        payload.get("certificateOidcIssuerRegexp"), field="certificateOidcIssuerRegexp"
    )
    _validate_policy_regexp(certificate_identity_regexp, field="certificateIdentityRegexp")
    _validate_policy_regexp(certificate_issuer_regexp, field="certificateOidcIssuerRegexp")
    _validate_exact_issuer(certificate_issuer)

    if mode == "ephemeral-key":
        valid = (
            not require_tlog
            and key_reference is None
            and not raw_environment
            and token_environment is None
            and certificate_identity is None
            and certificate_identity_regexp is None
            and certificate_issuer is None
            and certificate_issuer_regexp is None
        )
    elif mode == "kms-key":
        valid = (
            require_tlog
            and key_reference is not None
            and KMS_KEY_REFERENCE_PATTERN.fullmatch(key_reference) is not None
            and token_environment is None
            and certificate_identity is None
            and certificate_identity_regexp is None
            and certificate_issuer is None
            and certificate_issuer_regexp is None
        )
    else:
        valid = (
            require_tlog
            and key_reference is None
            and not raw_environment
            and token_environment is not None
            and ENVIRONMENT_NAME_PATTERN.fullmatch(token_environment) is not None
            and "TOKEN" in token_environment
            and (certificate_identity is None) != (certificate_identity_regexp is None)
            and (certificate_issuer is None) != (certificate_issuer_regexp is None)
            and (
                certificate_issuer_regexp is None
                or certificate_issuer_regexp.startswith("^https://")
            )
        )
    if not valid:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy fields did not match its selected mode.",
            {"mode": mode},
        )
    return SigningPolicy(
        mode=str(mode),
        require_transparency_log=require_tlog,
        key_reference=key_reference,
        credential_environment=tuple(raw_environment),
        identity_token_environment=token_environment,
        certificate_identity=certificate_identity,
        certificate_identity_regexp=certificate_identity_regexp,
        certificate_oidc_issuer=certificate_issuer,
        certificate_oidc_issuer_regexp=certificate_issuer_regexp,
        sha256=_sha256(raw),
    )


def load_vulnerability_policy(repo_root: pathlib.Path) -> VulnerabilityPolicy:
    raw = _read_bytes(repo_root, VULNERABILITY_POLICY_PATH, label="vulnerability policy")
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy was not valid JSON.",
        ) from None
    expected_keys = {
        "schemaVersion",
        "blockedSeverities",
        "ignoreUnfixed",
        "failOnEndOfLifeOS",
        "maximumDatabaseAgeHours",
        "exceptions",
    }
    if not isinstance(payload, dict) or set(payload) != expected_keys or payload.get("schemaVersion") != 1:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy schema was invalid.",
        )
    severities = payload.get("blockedSeverities")
    maximum_age = payload.get("maximumDatabaseAgeHours")
    raw_exceptions = payload.get("exceptions")
    if (
        not isinstance(severities, list)
        or not severities
        or not all(isinstance(value, str) and value in SUPPORTED_SEVERITIES for value in severities)
        or len(set(severities)) != len(severities)
        or not isinstance(payload.get("ignoreUnfixed"), bool)
        or not isinstance(payload.get("failOnEndOfLifeOS"), bool)
        or isinstance(maximum_age, bool)
        or not isinstance(maximum_age, int)
        or not 1 <= maximum_age <= 168
        or not isinstance(raw_exceptions, list)
    ):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy values were invalid.",
        )
    exceptions: list[VulnerabilityException] = []
    identities: set[tuple[str, str, str]] = set()
    exception_keys = {"vulnerabilityId", "package", "platform", "expiresAt", "owner", "reason"}
    for index, item in enumerate(raw_exceptions):
        if not isinstance(item, dict) or set(item) != exception_keys:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy exception schema was invalid.",
                {"exceptionIndex": index},
            )
        vulnerability_id = item.get("vulnerabilityId")
        package = item.get("package")
        platform = item.get("platform")
        owner = item.get("owner")
        reason = item.get("reason")
        if (
            not isinstance(vulnerability_id, str)
            or VULNERABILITY_ID_PATTERN.fullmatch(vulnerability_id) is None
            or not isinstance(package, str)
            or not package.strip()
            or len(package) > 256
            or platform not in {"linux/amd64", "linux/arm64"}
            or not isinstance(owner, str)
            or len(owner.strip()) < 2
            or len(owner) > 200
            or not isinstance(reason, str)
            or len(reason.strip()) < 10
            or len(reason) > 1000
        ):
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy exception values were invalid.",
                {"exceptionIndex": index},
            )
        exception = VulnerabilityException(
            vulnerability_id=vulnerability_id.upper(),
            package=package.strip(),
            platform=str(platform),
            expires_at=_parse_timestamp(item.get("expiresAt"), field=f"exceptions[{index}].expiresAt"),
            owner=owner.strip(),
            reason=reason.strip(),
        )
        if exception.identity in identities:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy contained duplicate exceptions.",
                {"exceptionIndex": index},
            )
        identities.add(exception.identity)
        exceptions.append(exception)
    return VulnerabilityPolicy(
        blocked_severities=tuple(severities),
        ignore_unfixed=bool(payload["ignoreUnfixed"]),
        fail_on_end_of_life_os=bool(payload["failOnEndOfLifeOS"]),
        maximum_database_age_hours=int(maximum_age),
        exceptions=tuple(exceptions),
        sha256=_sha256(raw),
    )


def load_configuration(repo_root: pathlib.Path) -> SupplyChainConfiguration:
    return SupplyChainConfiguration(
        tools=load_tool_images(repo_root),
        signing_policy=load_signing_policy(repo_root),
        vulnerability_policy=load_vulnerability_policy(repo_root),
    )


def _locked_version(reference: str, *, tool: str) -> str:
    named_reference = reference.rsplit("@", 1)[0]
    last_component = named_reference.rsplit("/", 1)[-1]
    if ":" not in last_component:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool reference omitted a human-readable version tag.",
            {"tool": tool},
        )
    version = last_component.rsplit(":", 1)[-1]
    if re.fullmatch(r"v?[0-9]+\.[0-9]+\.[0-9]+", version) is None:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool reference used an invalid version tag.",
            {"tool": tool},
        )
    return version


def _remaining(deadline: float, *, maximum: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_timeout",
            "Worker Registry supply-chain verification exceeded its deadline.",
        )
    return max(1.0, min(maximum, remaining))


def _run_tool(
    options: SupplyChainOptions,
    *,
    image: str,
    arguments: Sequence[str],
    tool: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
    secret_environment: Mapping[str, str] | None = None,
    maximum_timeout: float = 900.0,
) -> subprocess.CompletedProcess[str]:
    options.state_dir.mkdir(parents=True, exist_ok=True)
    (options.state_dir / "tool-home").mkdir(parents=True, exist_ok=True)
    command = [
        options.docker_bin,
        "run",
        "--rm",
        "--network",
        "host",
        "--user",
        f"{os.getuid()}:{os.getgid()}",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--env",
        "HOME=/workspace/tool-home",
        "--volume",
        f"{options.state_dir}:/workspace",
        "--workdir",
        "/workspace",
    ]
    environment = remote.tool_environment()
    for name, value in (secret_environment or {}).items():
        environment[name] = value
        command.extend(["--env", name])
    command.extend([image, *arguments])
    try:
        completed = subprocess.run(
            command,
            cwd=options.repo_root,
            env=environment,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=_remaining(deadline, maximum=maximum_timeout),
        )
    except (OSError, subprocess.TimeoutExpired):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "A digest-pinned Worker Registry supply-chain tool could not complete.",
            {"tool": tool},
        ) from None
    if completed.returncode != 0:
        output = redactor.text((completed.stdout + "\n" + completed.stderr).strip())[:2000]
        raise common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "A digest-pinned Worker Registry supply-chain tool returned a failure.",
            {"tool": tool, "returnCode": completed.returncode, "outputExcerpt": output},
        )
    return completed


def _retryable_trivy_database_download(error: common.ReleaseGateError) -> bool:
    output = error.evidence.get("outputExcerpt")
    if (
        error.code != "release.registry_supply_chain_command_failed"
        or error.evidence.get("tool") != "trivy"
        or not isinstance(output, str)
    ):
        return False
    normalized = output.lower()
    return "failed to download vulnerability db" in normalized and any(
        marker in normalized for marker in TRIVY_DATABASE_DOWNLOAD_RETRY_MARKERS
    )


def _run_trivy_scan(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    arguments: Sequence[str],
    report_path: pathlib.Path,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> int:
    for attempt in range(2):
        try:
            _run_tool(
                options,
                image=configuration.tools.trivy,
                arguments=arguments,
                tool="trivy",
                deadline=deadline,
                redactor=redactor,
                maximum_timeout=1200.0,
            )
            return attempt
        except common.ReleaseGateError as error:
            if attempt > 0 or not _retryable_trivy_database_download(error):
                raise
            report_path.unlink(missing_ok=True)
            if deadline - time.monotonic() <= TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS:
                raise
            time.sleep(TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS)
    raise AssertionError("unreachable")


def _load_json_file(path: pathlib.Path, *, code: str, message: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise common.ReleaseGateError(code, message) from None


def validate_cosign_verification(
    payload: Any,
    *,
    reference: str,
    digest: str,
    annotations: Mapping[str, str],
) -> dict[str, Any]:
    if not isinstance(payload, list) or not payload or not all(isinstance(item, dict) for item in payload):
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign verification did not return a valid signature claim list.",
        )
    matching: list[dict[str, Any]] = []
    for item in payload:
        critical = item.get("critical")
        optional = item.get("optional")
        identity = critical.get("identity") if isinstance(critical, dict) else None
        image = critical.get("image") if isinstance(critical, dict) else None
        if (
            isinstance(identity, dict)
            and isinstance(image, dict)
            and identity.get("docker-reference") == reference
            and image.get("docker-manifest-digest") == digest
            and critical.get("type") == COSIGN_CLAIM_TYPE
            and isinstance(optional, dict)
            and all(optional.get(key) == value for key, value in annotations.items())
        ):
            matching.append(item)
    if len(matching) != 1:
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign did not return exactly one signature for the expected digest and source annotations.",
            {"matchingSignatures": len(matching)},
        )
    return {
        "verifiedSignatureCount": 1,
        "claimType": COSIGN_CLAIM_TYPE,
        "annotations": dict(annotations),
        "verificationPayloadSha256": _sha256(
            json.dumps(matching, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
    }


def _signature_subjects(
    options: SupplyChainOptions,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
) -> list[dict[str, Any]]:
    subjects: list[dict[str, Any]] = []
    for build in builds:
        slot = build.get("slot")
        digest = build.get("registryDigest")
        if not isinstance(slot, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", str(digest)) is None:
            raise common.ReleaseGateError(
                "release.registry_signature_input_invalid",
                "Worker Registry signature input omitted a valid slot or registry digest.",
            )
        reference = f"{options.image_repository}@{digest}"
        annotations = {
            "synara.git-sha": git_sha,
            "synara.run-id": run_id,
            "synara.slot": slot,
            "synara.version": version,
        }
        subjects.append(
            {
                "slot": slot,
                "digest": str(digest),
                "reference": reference,
                "annotations": annotations,
                "annotationArguments": [
                    value
                    for key, item in annotations.items()
                    for value in ("-a", f"{key}={item}")
                ],
            }
        )
    return subjects


def _verification_evidence(
    completed: subprocess.CompletedProcess[str],
    *,
    subject: Mapping[str, Any],
) -> dict[str, Any]:
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign verification output was not valid JSON.",
            {"slot": subject.get("slot")},
        ) from None
    return {
        "slot": subject["slot"],
        "reference": subject["reference"],
        "digest": subject["digest"],
        **validate_cosign_verification(
            payload,
            reference=str(subject["reference"]),
            digest=str(subject["digest"]),
            annotations=subject["annotations"],
        ),
    }


def _require_production_registry(options: SupplyChainOptions) -> None:
    if options.insecure_registry:
        raise common.ReleaseGateError(
            "release.registry_production_signing_insecure_registry",
            "Production Worker Registry signing requires a TLS Registry.",
        )


def _read_signing_environment(
    names: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, str]:
    values: dict[str, str] = {}
    for name in names:
        try:
            value = acceptance.read_environment_value(
                name,
                "Cosign KMS credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            )
        except acceptance.EnvironmentValueError as error:
            raise common.ReleaseGateError(
                "release.registry_signing_credential_invalid",
                "A configured Cosign KMS credential environment value was unavailable or invalid.",
                {"reason": error.reason},
            ) from None
        redactor.add(value, "[REDACTED_COSIGN_KMS_CREDENTIAL]")
        values[name] = value
    return values


def _sign_and_verify_ephemeral(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    cosign_dir = options.state_dir / "cosign"
    cosign_dir.mkdir(parents=True, exist_ok=True)
    signing_config = pathlib.Path("cosign/signing-config.json")
    key_prefix = pathlib.Path("cosign/ephemeral")
    private_key = options.state_dir / "cosign" / "ephemeral.key"
    public_key = options.state_dir / "cosign" / "ephemeral.pub"
    passphrase = secrets.token_urlsafe(48)
    redactor.add(passphrase, "[REDACTED_EPHEMERAL_COSIGN_PASSWORD]")
    secret_environment = {"COSIGN_PASSWORD": passphrase}
    signatures: list[dict[str, Any]] = []
    public_key_sha256: str | None = None
    try:
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "signing-config",
                "create",
                "--no-default-fulcio",
                "--no-default-oidc",
                "--no-default-rekor",
                "--no-default-tsa",
                "--out",
                str(signing_config),
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
        )
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=["generate-key-pair", "--output-key-prefix", str(key_prefix)],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=secret_environment,
        )
        if not private_key.is_file() or not public_key.is_file():
            raise common.ReleaseGateError(
                "release.registry_signature_key_invalid",
                "Cosign did not create the isolated ephemeral key pair.",
            )
        public_key_sha256 = _sha256(public_key.read_bytes())
        for subject in _signature_subjects(
            options,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
        ):
            insecure_arguments = ["--allow-insecure-registry"] if options.insecure_registry else []
            _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "sign",
                    "--yes",
                    "--signing-config",
                    str(signing_config),
                    *insecure_arguments,
                    "--key",
                    str(key_prefix.with_suffix(".key")),
                    *subject["annotationArguments"],
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
                secret_environment=secret_environment,
            )
            verification = _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "verify",
                    *insecure_arguments,
                    "--insecure-ignore-tlog=true",
                    "--key",
                    str(key_prefix.with_suffix(".pub")),
                    *subject["annotationArguments"],
                    "--output",
                    "json",
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            signatures.append(_verification_evidence(verification, subject=subject))
    finally:
        private_key.unlink(missing_ok=True)
    private_key_removed = not private_key.exists()
    if not private_key_removed:
        raise common.ReleaseGateError(
            "release.registry_signature_key_cleanup_failed",
            "Worker Registry supply-chain gate did not remove its ephemeral private key.",
        )
    return {
        "mode": "ephemeral-key",
        "transparencyLog": False,
        "productionSigningPolicySatisfied": False,
        "policySha256": configuration.signing_policy.sha256,
        "publicKeySha256": public_key_sha256,
        "signatures": signatures,
        "privateKeyRemoved": private_key_removed,
    }


def _keyless_verification_arguments(policy: SigningPolicy) -> list[str]:
    identity_arguments = (
        ["--certificate-identity", policy.certificate_identity]
        if policy.certificate_identity is not None
        else ["--certificate-identity-regexp", policy.certificate_identity_regexp]
    )
    issuer_arguments = (
        ["--certificate-oidc-issuer", policy.certificate_oidc_issuer]
        if policy.certificate_oidc_issuer is not None
        else ["--certificate-oidc-issuer-regexp", policy.certificate_oidc_issuer_regexp]
    )
    return [str(value) for value in (*identity_arguments, *issuer_arguments)]


def _sign_and_verify_keyless(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    _require_production_registry(options)
    policy = configuration.signing_policy
    token_environment = policy.identity_token_environment
    if token_environment is None:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry keyless signing policy omitted its identity token environment.",
        )
    try:
        identity_token = acceptance.read_environment_value(
            token_environment,
            "Cosign keyless identity token",
            maximum_length=1 << 20,
            forbidden_characters="\r\n\x00",
        )
    except acceptance.EnvironmentValueError as error:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured Cosign keyless identity token was unavailable or invalid.",
            {"reason": error.reason},
        ) from None
    redactor.add(identity_token, "[REDACTED_COSIGN_IDENTITY_TOKEN]")
    token_relative = pathlib.Path("cosign/identity-token")
    token_path = options.state_dir / token_relative
    token_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        token_path.write_text(identity_token, encoding="utf-8")
        token_path.chmod(0o600)
    except OSError:
        token_path.unlink(missing_ok=True)
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry gate could not create isolated keyless identity-token state.",
        ) from None
    signatures: list[dict[str, Any]] = []
    try:
        for subject in _signature_subjects(
            options,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
        ):
            _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "sign",
                    "--yes",
                    "--tlog-upload=true",
                    "--identity-token",
                    str(token_relative),
                    *subject["annotationArguments"],
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            verification = _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "verify",
                    *_keyless_verification_arguments(policy),
                    *subject["annotationArguments"],
                    "--output",
                    "json",
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            signatures.append(_verification_evidence(verification, subject=subject))
    finally:
        token_path.unlink(missing_ok=True)
    token_removed = not token_path.exists()
    if not token_removed:
        raise common.ReleaseGateError(
            "release.registry_signature_key_cleanup_failed",
            "Worker Registry supply-chain gate did not remove its keyless identity token.",
        )
    return {
        "mode": "keyless",
        "transparencyLog": True,
        "transparencyLogVerified": True,
        "productionSigningPolicySatisfied": True,
        "policySha256": policy.sha256,
        **{
            key: value
            for key, value in policy.as_report().items()
            if key.startswith("certificate")
        },
        "signatures": signatures,
        "identityTokenRemoved": token_removed,
    }


def _sign_and_verify_kms(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    _require_production_registry(options)
    policy = configuration.signing_policy
    key_reference = policy.key_reference
    if key_reference is None:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry KMS signing policy omitted its key reference.",
        )
    secret_environment = _read_signing_environment(
        policy.credential_environment,
        redactor=redactor,
    )
    signatures: list[dict[str, Any]] = []
    for subject in _signature_subjects(
        options,
        builds=builds,
        git_sha=git_sha,
        version=version,
        run_id=run_id,
    ):
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "sign",
                "--yes",
                "--tlog-upload=true",
                "--key",
                key_reference,
                *subject["annotationArguments"],
                subject["reference"],
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=secret_environment,
        )
        verification = _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "verify",
                "--key",
                key_reference,
                *subject["annotationArguments"],
                "--output",
                "json",
                subject["reference"],
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=secret_environment,
        )
        signatures.append(_verification_evidence(verification, subject=subject))
    return {
        "mode": "kms-key",
        "transparencyLog": True,
        "transparencyLogVerified": True,
        "productionSigningPolicySatisfied": True,
        "policySha256": policy.sha256,
        "keyReference": key_reference,
        "credentialEnvironmentCount": len(policy.credential_environment),
        "signatures": signatures,
    }


def _sign_and_verify(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    policy = configuration.signing_policy
    if policy.mode == "ephemeral-key":
        return _sign_and_verify_ephemeral(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    if policy.mode == "keyless":
        return _sign_and_verify_keyless(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    if policy.mode == "kms-key":
        return _sign_and_verify_kms(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    raise common.ReleaseGateError(
        "release.registry_signing_policy_invalid",
        "Worker Registry signing policy selected an unsupported mode.",
    )


def _vulnerability_summary(vulnerabilities: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    by_severity = {severity: 0 for severity in SUPPORTED_SEVERITIES}
    fixable = 0
    for vulnerability in vulnerabilities:
        severity = vulnerability.get("Severity")
        if severity in by_severity:
            by_severity[str(severity)] += 1
        fixed_version = vulnerability.get("FixedVersion")
        if isinstance(fixed_version, str) and fixed_version.strip():
            fixable += 1
    return {
        "total": len(vulnerabilities),
        "fixable": fixable,
        "unfixed": len(vulnerabilities) - fixable,
        "bySeverity": by_severity,
    }


def _exception_matches(
    exception: VulnerabilityException,
    vulnerability: Mapping[str, Any],
    *,
    platform: str,
) -> bool:
    vulnerability_id = vulnerability.get("VulnerabilityID")
    package = vulnerability.get("PkgName")
    return (
        isinstance(vulnerability_id, str)
        and vulnerability_id.upper() == exception.vulnerability_id
        and package == exception.package
        and platform == exception.platform
    )


def _safe_vulnerability(vulnerability: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "vulnerabilityId": vulnerability.get("VulnerabilityID"),
        "package": vulnerability.get("PkgName"),
        "installedVersion": vulnerability.get("InstalledVersion"),
        "fixedVersion": vulnerability.get("FixedVersion"),
        "severity": vulnerability.get("Severity"),
        "status": vulnerability.get("Status"),
        "primaryUrl": vulnerability.get("PrimaryURL"),
        "target": vulnerability.get("_Target"),
        "class": vulnerability.get("_Class"),
        "type": vulnerability.get("_Type"),
    }


def evaluate_trivy_report(
    payload: Any,
    *,
    platform: str,
    reference: str,
    policy: VulnerabilityPolicy,
    now: dt.datetime,
) -> tuple[dict[str, Any], list[dict[str, Any]], set[tuple[str, str, str]]]:
    if not isinstance(payload, dict) or payload.get("SchemaVersion") != 2:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy did not produce the expected JSON report schema.",
            {"platform": platform},
        )
    metadata = payload.get("Metadata")
    repo_digests = metadata.get("RepoDigests") if isinstance(metadata, dict) else None
    if (
        payload.get("ArtifactName") != reference
        or payload.get("ArtifactType") != "container_image"
        or not isinstance(repo_digests, list)
        or reference not in repo_digests
    ):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy report identity did not match the requested immutable platform digest.",
            {"platform": platform},
        )
    results = payload.get("Results")
    if not isinstance(results, list) or not all(isinstance(item, dict) for item in results):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy report omitted its result list.",
            {"platform": platform},
        )
    vulnerabilities = [
        {
            **vulnerability,
            "_Target": result.get("Target"),
            "_Class": result.get("Class"),
            "_Type": result.get("Type"),
        }
        for result in results
        for vulnerability in (result.get("Vulnerabilities") or [])
        if isinstance(vulnerability, dict)
    ]
    secret_findings = [
        secret
        for result in results
        for secret in (result.get("Secrets") or [])
        if isinstance(secret, dict)
    ]
    errors: list[dict[str, Any]] = []
    used_exceptions: set[tuple[str, str, str]] = set()
    blocked: list[dict[str, Any]] = []
    waived: list[dict[str, Any]] = []
    expired: list[dict[str, Any]] = []
    for vulnerability in vulnerabilities:
        severity = vulnerability.get("Severity")
        fixed_version = vulnerability.get("FixedVersion")
        if severity not in policy.blocked_severities:
            continue
        if policy.ignore_unfixed and not (isinstance(fixed_version, str) and fixed_version.strip()):
            continue
        matching = [
            exception
            for exception in policy.exceptions
            if _exception_matches(exception, vulnerability, platform=platform)
        ]
        active = [exception for exception in matching if exception.expires_at > now]
        finding = _safe_vulnerability(vulnerability)
        if active:
            exception = active[0]
            used_exceptions.add(exception.identity)
            waived.append(
                {
                    **finding,
                    "owner": exception.owner,
                    "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
                    "reason": exception.reason,
                }
            )
        else:
            blocked.append(finding)
            for exception in matching:
                used_exceptions.add(exception.identity)
                expired.append(
                    {
                        "vulnerabilityId": exception.vulnerability_id,
                        "package": exception.package,
                        "platform": exception.platform,
                        "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
                    }
                )
    os_metadata = metadata.get("OS") if isinstance(metadata, dict) else None
    end_of_life = os_metadata.get("EOSL") if isinstance(os_metadata, dict) else None
    if policy.fail_on_end_of_life_os and end_of_life is True:
        errors.append(
            {
                "code": "release.registry_vulnerability_os_eol",
                "message": "Worker Registry image used an end-of-life operating-system release.",
                "evidence": {"platform": platform, "os": os_metadata},
            }
        )
    if secret_findings:
        safe_findings = [
            {
                key: finding.get(key)
                for key in ("RuleID", "Category", "Title", "Target", "StartLine", "EndLine")
                if key in finding
            }
            for finding in secret_findings[:50]
        ]
        errors.append(
            {
                "code": "release.registry_image_secret_detected",
                "message": "Trivy found Secret-like material in the Worker Registry image.",
                "evidence": {
                    "platform": platform,
                    "findingCount": len(secret_findings),
                    "findings": safe_findings,
                },
            }
        )
    if expired:
        errors.append(
            {
                "code": "release.registry_vulnerability_exception_expired",
                "message": "Worker Registry vulnerability policy contained an expired matching exception.",
                "evidence": {"platform": platform, "exceptions": expired},
            }
        )
    if blocked:
        errors.append(
            {
                "code": "release.registry_vulnerability_policy_blocked",
                "message": "Worker Registry image violated the checked-in vulnerability policy.",
                "evidence": {
                    "platform": platform,
                    "findingCount": len(blocked),
                    "findings": blocked[:100],
                },
            }
        )
    evidence = {
        "platform": platform,
        "reference": reference,
        "artifactId": metadata.get("ImageID") if isinstance(metadata, dict) else None,
        "os": os_metadata,
        "vulnerabilities": _vulnerability_summary(vulnerabilities),
        "reviewFindings": sorted(
            (
                _safe_vulnerability(vulnerability)
                for vulnerability in vulnerabilities
                if vulnerability.get("Severity") in {"UNKNOWN", "HIGH", "CRITICAL"}
            ),
            key=lambda finding: (
                str(finding.get("severity")),
                str(finding.get("vulnerabilityId")),
                str(finding.get("package")),
            ),
        ),
        "reviewFindingCount": sum(
            1
            for vulnerability in vulnerabilities
            if vulnerability.get("Severity") in {"UNKNOWN", "HIGH", "CRITICAL"}
        ),
        "blockedFindings": blocked,
        "waivedFindings": waived,
        "secretFindingCount": len(secret_findings),
        "reportSha256": _sha256(
            json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
    }
    return evidence, errors, used_exceptions


def evaluate_trivy_database(
    payload: Any,
    *,
    expected_version: str,
    policy: VulnerabilityPolicy,
    now: dt.datetime,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    if not isinstance(payload, dict) or payload.get("Version") != expected_version:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy version output did not match the checked-in tool lock.",
        )
    database = payload.get("VulnerabilityDB")
    if not isinstance(database, dict):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy did not report vulnerability database metadata.",
        )
    updated_at = _parse_timestamp(database.get("UpdatedAt"), field="VulnerabilityDB.UpdatedAt")
    age_seconds = (now - updated_at).total_seconds()
    errors: list[dict[str, Any]] = []
    if age_seconds < -300 or age_seconds > policy.maximum_database_age_hours * 3600:
        errors.append(
            {
                "code": "release.registry_vulnerability_database_stale",
                "message": "Trivy vulnerability database was outside the checked-in freshness policy.",
                "evidence": {
                    "updatedAt": updated_at.isoformat().replace("+00:00", "Z"),
                    "ageSeconds": int(age_seconds),
                    "maximumAgeHours": policy.maximum_database_age_hours,
                },
            }
        )
    return (
        {
            "toolVersion": expected_version,
            "schemaVersion": database.get("Version"),
            "updatedAt": updated_at.isoformat().replace("+00:00", "Z"),
            "nextUpdate": database.get("NextUpdate"),
            "downloadedAt": database.get("DownloadedAt"),
            "ageSeconds": int(age_seconds),
            "maximumAgeHours": policy.maximum_database_age_hours,
        },
        errors,
    )


def _scan_platforms(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    platform_digests: Mapping[str, Any],
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    policy = configuration.vulnerability_policy
    scans: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    used_exceptions: set[tuple[str, str, str]] = set()
    transient_database_download_retries = 0
    now = dt.datetime.now(tz=dt.timezone.utc)
    for platform in ("linux/amd64", "linux/arm64"):
        digest = platform_digests.get(platform)
        if re.fullmatch(r"sha256:[0-9a-f]{64}", str(digest)) is None:
            raise common.ReleaseGateError(
                "release.registry_vulnerability_input_invalid",
                "Worker Registry vulnerability scan omitted a required platform digest.",
                {"platform": platform},
            )
        reference = f"{options.image_repository}@{digest}"
        report_name = f"trivy-{platform.replace('/', '-')}.json"
        report_path = options.state_dir / report_name
        insecure_arguments = ["--insecure"] if options.insecure_registry else []
        scan_timeout = int(_remaining(deadline, maximum=1200.0))
        arguments = [
            "image",
            "--quiet",
            "--image-src",
            "remote",
            *insecure_arguments,
            "--skip-version-check",
            "--cache-dir",
            "/workspace/trivy-cache",
            "--timeout",
            f"{max(60, scan_timeout)}s",
            "--scanners",
            "vuln,secret",
            "--severity",
            ",".join(SUPPORTED_SEVERITIES),
            "--format",
            "json",
            "--output",
            f"/workspace/{report_name}",
        ]
        if policy.ignore_unfixed:
            arguments.append("--ignore-unfixed")
        arguments.append(reference)
        transient_database_download_retries += _run_trivy_scan(
            options,
            configuration,
            arguments=arguments,
            report_path=report_path,
            deadline=deadline,
            redactor=redactor,
        )
        payload = _load_json_file(
            report_path,
            code="release.registry_vulnerability_report_invalid",
            message="Trivy did not write a valid Worker Registry vulnerability report.",
        )
        evidence, platform_errors, platform_exceptions = evaluate_trivy_report(
            payload,
            platform=platform,
            reference=reference,
            policy=policy,
            now=now,
        )
        scans.append(evidence)
        errors.extend(platform_errors)
        used_exceptions.update(platform_exceptions)
        report_path.unlink(missing_ok=True)
    stale_exceptions = [
        {
            "vulnerabilityId": exception.vulnerability_id,
            "package": exception.package,
            "platform": exception.platform,
            "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
            "owner": exception.owner,
        }
        for exception in policy.exceptions
        if exception.identity not in used_exceptions
    ]
    if stale_exceptions:
        errors.append(
            {
                "code": "release.registry_vulnerability_exception_stale",
                "message": "Worker Registry vulnerability policy contained an unused exception.",
                "evidence": {"exceptions": stale_exceptions},
            }
        )
    version = _run_tool(
        options,
        image=configuration.tools.trivy,
        arguments=[
            "--cache-dir",
            "/workspace/trivy-cache",
            "--version",
            "--format",
            "json",
        ],
        tool="trivy",
        deadline=deadline,
        redactor=redactor,
    )
    try:
        version_payload = json.loads(version.stdout)
    except json.JSONDecodeError:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy version output was not valid JSON.",
        ) from None
    expected_version = _locked_version(configuration.tools.trivy, tool="trivy").removeprefix("v")
    database, database_errors = evaluate_trivy_database(
        version_payload,
        expected_version=expected_version,
        policy=policy,
        now=now,
    )
    errors.extend(database_errors)
    return (
        {
            "policy": policy.as_report(),
            "database": database,
            "scans": scans,
            "staleExceptionCount": len(stale_exceptions),
            "transientDatabaseDownloadRetries": transient_database_download_retries,
        },
        errors,
    )


def _tool_versions(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, str]:
    cosign = _run_tool(
        options,
        image=configuration.tools.cosign,
        arguments=["version"],
        tool="cosign",
        deadline=deadline,
        redactor=redactor,
        maximum_timeout=300.0,
    )
    match = re.search(r"(?m)^GitVersion:\s+(v[0-9]+\.[0-9]+\.[0-9]+)\s*$", cosign.stdout)
    expected_cosign = _locked_version(configuration.tools.cosign, tool="cosign")
    if match is None or match.group(1) != expected_cosign:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_tool_version_invalid",
            "Cosign runtime version did not match the checked-in digest lock.",
        )
    trivy = _run_tool(
        options,
        image=configuration.tools.trivy,
        arguments=["--version", "--format", "json"],
        tool="trivy",
        deadline=deadline,
        redactor=redactor,
        maximum_timeout=300.0,
    )
    try:
        trivy_payload = json.loads(trivy.stdout)
    except json.JSONDecodeError:
        trivy_payload = None
    expected_trivy = _locked_version(configuration.tools.trivy, tool="trivy").removeprefix("v")
    if not isinstance(trivy_payload, dict) or trivy_payload.get("Version") != expected_trivy:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_tool_version_invalid",
            "Trivy runtime version did not match the checked-in digest lock.",
        )
    return {"cosign": expected_cosign, "trivy": expected_trivy}


def verify_supply_chain(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    started = time.monotonic()
    deadline = started + options.timeout_seconds
    errors: list[dict[str, Any]] = []
    versions: dict[str, str] = {}
    signing: dict[str, Any] = {}
    vulnerability: dict[str, Any] = {}
    try:
        versions = _tool_versions(
            options,
            configuration,
            deadline=deadline,
            redactor=redactor,
        )
    except common.ReleaseGateError as error:
        errors.append(error.as_report_error())
    tools_ready = not errors
    if tools_ready:
        try:
            signing = _sign_and_verify(
                options,
                configuration,
                builds=builds,
                git_sha=git_sha,
                version=version,
                run_id=run_id,
                deadline=deadline,
                redactor=redactor,
            )
        except common.ReleaseGateError as error:
            errors.append(error.as_report_error())
    platform_digests = builds[0].get("platformDigests") if builds else None
    if tools_ready and isinstance(platform_digests, dict):
        try:
            vulnerability, vulnerability_errors = _scan_platforms(
                options,
                configuration,
                platform_digests=platform_digests,
                deadline=deadline,
                redactor=redactor,
            )
            errors.extend(vulnerability_errors)
        except common.ReleaseGateError as error:
            errors.append(error.as_report_error())
    private_key = options.state_dir / "cosign" / "ephemeral.key"
    identity_token = options.state_dir / "cosign" / "identity-token"
    private_key.unlink(missing_ok=True)
    identity_token.unlink(missing_ok=True)
    private_key_removed = not private_key.exists()
    identity_token_removed = not identity_token.exists()
    signing_secret_state_removed = private_key_removed and identity_token_removed
    if signing:
        signing["privateKeyRemoved"] = private_key_removed
        signing["identityTokenRemoved"] = identity_token_removed
        signing["secretStateRemoved"] = signing_secret_state_removed
    if not signing_secret_state_removed:
        errors.append(
            {
                "code": "release.registry_signature_key_cleanup_failed",
                "message": "Worker Registry supply-chain gate did not remove isolated signing Secret state.",
            }
        )
    return {
        "status": "pass" if not errors else "fail",
        "mode": "registry-supply-chain",
        "tools": {**configuration.tools.as_report(), "versions": versions},
        "signing": signing,
        "vulnerability": vulnerability,
        "cleanup": {
            "ephemeralPrivateKeyRemoved": private_key_removed,
            "identityTokenRemoved": identity_token_removed,
            "signingSecretStateRemoved": signing_secret_state_removed,
            "broadCleanupUsed": False,
        },
        "durationMs": acceptance.elapsed_ms(started),
        "errors": errors,
    }
