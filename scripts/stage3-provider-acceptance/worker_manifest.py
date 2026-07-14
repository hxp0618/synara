#!/usr/bin/env python3
"""Build deterministic Worker Protocol v2 acceptance capabilities from the shared Provider catalog."""

from __future__ import annotations

import argparse
import json
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any


WORKER_PROTOCOL_VERSION = 2
RUNTIME_EVENT_VERSION = 2
PROVIDER_HOST_PROTOCOL_MAJOR = 2
PROVIDER_HOST_PROTOCOL_MINOR = 1
DEFAULT_EXPERIMENTAL_PROVIDERS = ("codex", "claudeAgent")


def default_catalog_path() -> Path:
    return Path(__file__).resolve().parents[2] / "packages/contracts/src/providerCapabilityCatalog.json"


def load_json_object(path: Path, label: str) -> dict[str, Any]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be a JSON object")
    return value


def build_worker_capabilities(
    catalog: Mapping[str, Any],
    target_capabilities: Mapping[str, Any],
    *,
    worker_version: str,
) -> dict[str, Any]:
    normalized_worker_version = _non_empty_string(worker_version, "worker version")
    capability_ids = _string_sequence(catalog.get("capabilityIds"), "catalog capabilityIds")
    provider_entries = catalog.get("providers")
    if catalog.get("version") != 1 or not isinstance(provider_entries, Sequence) or not provider_entries:
        raise ValueError("Provider capability catalog is invalid")

    provider_policy = target_capabilities.get("providerPolicy", {})
    if not isinstance(provider_policy, Mapping):
        raise ValueError("target providerPolicy must be an object")
    enabled_experimental = set(
        _string_sequence(provider_policy.get("experimentalProviders", []), "experimentalProviders")
    )

    providers: dict[str, Any] = {}
    for raw_entry in provider_entries:
        if not isinstance(raw_entry, Mapping):
            raise ValueError("Provider catalog entries must be objects")
        provider = _non_empty_string(raw_entry.get("provider"), "provider")
        if provider in providers:
            raise ValueError(f"Provider catalog contains duplicate provider {provider!r}")
        support_tier = _non_empty_string(raw_entry.get("supportTier"), f"{provider} supportTier")
        adapter_version = _non_empty_string(raw_entry.get("adapterVersion"), f"{provider} adapterVersion")
        runtime_policy = raw_entry.get("runtimePolicy")
        provider_capabilities = raw_entry.get("capabilities")
        if not isinstance(runtime_policy, Mapping) or not isinstance(provider_capabilities, Mapping):
            raise ValueError(f"Provider {provider!r} catalog entry is incomplete")
        if set(provider_capabilities) != set(capability_ids):
            raise ValueError(f"Provider {provider!r} capability matrix does not match capabilityIds")

        compatible_range = runtime_policy.get("compatibleRange")
        if not isinstance(compatible_range, Mapping):
            raise ValueError(f"Provider {provider!r} compatibleRange must be an object")
        minimum_version = _non_empty_string(
            compatible_range.get("minimumInclusive"), f"{provider} minimumInclusive"
        )
        normalized_range: dict[str, str] = {"minimumInclusive": minimum_version}
        maximum_version = compatible_range.get("maximumExclusive")
        if maximum_version is not None:
            normalized_range["maximumExclusive"] = _non_empty_string(
                maximum_version, f"{provider} maximumExclusive"
            )

        available = support_tier != "local-only"
        runtime: dict[str, Any] = {
            "kind": _non_empty_string(runtime_policy.get("kind"), f"{provider} runtime kind"),
            "name": _non_empty_string(runtime_policy.get("name"), f"{provider} runtime name"),
            "available": available,
            "versionSource": _non_empty_string(
                runtime_policy.get("versionSource"), f"{provider} versionSource"
            ),
            "compatibleRange": normalized_range,
            "compatible": available,
        }
        if available:
            runtime["version"] = minimum_version

        experimental = support_tier == "experimental"
        descriptor: dict[str, Any] = {
            "provider": provider,
            "supportTier": support_tier,
            "adapterVersion": adapter_version,
            "runtime": runtime,
            "releasePolicy": {
                "requiresExplicitEnablement": experimental,
                "enabled": provider in enabled_experimental if experimental else True,
            },
            "capabilities": {
                capability_id: _non_empty_string(
                    provider_capabilities[capability_id], f"{provider} capability {capability_id}"
                )
                for capability_id in capability_ids
            },
        }
        if provider == "codex" and available:
            descriptor["providerCliVersion"] = minimum_version

        providers[provider] = {
            "protocolVersion": {
                "major": PROVIDER_HOST_PROTOCOL_MAJOR,
                "minor": PROVIDER_HOST_PROTOCOL_MINOR,
            },
            "hostBuildVersion": "acceptance-fixture",
            "maximumCommandBytes": 2 << 20,
            "maximumMessageBytes": 1 << 20,
            "runtimeEventVersions": {
                "minimum": RUNTIME_EVENT_VERSION,
                "maximum": RUNTIME_EVENT_VERSION,
            },
            "credentialDeliveryModes": ["anonymous-fd"],
            "resumeStrategies": ["native-cursor", "authoritative-history"],
            "capabilityDescriptor": descriptor,
        }

    return {
        "workerRuntime": {
            "workerBuildVersion": normalized_worker_version,
            "workerBuildGitSha": "0000000000000000",
            "workerProtocolMinimum": WORKER_PROTOCOL_VERSION,
            "workerProtocolMaximum": WORKER_PROTOCOL_VERSION,
            "runtimeEventMinimum": RUNTIME_EVENT_VERSION,
            "runtimeEventMaximum": RUNTIME_EVENT_VERSION,
            "operatingSystem": "linux",
            "architecture": "amd64",
        },
        "providerHost": {
            "protocolVersion": {
                "major": PROVIDER_HOST_PROTOCOL_MAJOR,
                "minor": PROVIDER_HOST_PROTOCOL_MINOR,
            },
            "legacy": False,
            "providers": providers,
        },
    }


def _string_sequence(value: Any, label: str) -> list[str]:
    if not isinstance(value, Sequence) or isinstance(value, (str, bytes)):
        raise ValueError(f"{label} must be an array")
    result = [_non_empty_string(item, label) for item in value]
    if len(result) != len(set(result)):
        raise ValueError(f"{label} must not contain duplicates")
    return result


def _non_empty_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{label} must be a non-empty string")
    return value.strip()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--catalog", type=Path, default=default_catalog_path())
    parser.add_argument("--target-capabilities-json")
    parser.add_argument("--worker-version", required=True)
    arguments = parser.parse_args()

    target_capabilities: dict[str, Any]
    if arguments.target_capabilities_json is None:
        target_capabilities = {
            "providerPolicy": {"experimentalProviders": list(DEFAULT_EXPERIMENTAL_PROVIDERS)}
        }
    else:
        value = json.loads(arguments.target_capabilities_json)
        if not isinstance(value, dict):
            raise ValueError("target capabilities must be a JSON object")
        target_capabilities = value

    capabilities = build_worker_capabilities(
        load_json_object(arguments.catalog, "Provider capability catalog"),
        target_capabilities,
        worker_version=arguments.worker_version,
    )
    print(json.dumps(capabilities, separators=(",", ":"), sort_keys=True))


if __name__ == "__main__":
    main()
