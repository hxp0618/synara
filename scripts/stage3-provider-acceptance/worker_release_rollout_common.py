from __future__ import annotations

import dataclasses
import json
import pathlib
import re
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance


ROLLOUT_LABEL = "synara.io/stage3-worker-release-rollout"
OWNER_LABEL = "synara.io/stage3-worker-release-rollout-owner"
SLOT_LABEL = "synara.io/stage3-worker-release-rollout-slot"
EXPECTED_POLICY_ACTIONS = (
    (1, "promote"),
    (2, "canary"),
    (3, "promote"),
    (4, "rollback"),
)
EXPECTED_AUDIT_ACTIONS = {
    1: "worker_release.promoted",
    2: "worker_release.canary_started",
    3: "worker_release.promoted",
    4: "worker_release.rolled_back",
}
EXPECTED_OUTBOX_TOPICS = {
    1: "worker.release.promoted",
    2: "worker.release.canary-started",
    3: "worker.release.promoted",
    4: "worker.release.rolled-back",
}


@dataclasses.dataclass(frozen=True)
class ReleaseImage:
    slot: str
    version: str
    tag: str
    exact_reference: str
    digest: str
    image_id: str
    metadata_path: pathlib.Path


class PendingReleaseEvidence(Exception):
    pass


def rollout_version(base_version: str, slot: str) -> str:
    suffix = f"rollout.{slot}"
    return f"{base_version}.{suffix}" if "+" in base_version else f"{base_version}+{suffix}"


def normalize_engine_platform(value: str) -> str:
    normalized = value.strip().lower()
    aliases = {
        "linux/x86_64": "linux/amd64",
        "linux/aarch64": "linux/arm64",
    }
    normalized = aliases.get(normalized, normalized)
    if normalized not in {"linux/amd64", "linux/arm64"}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_platform_unsupported",
            "The Worker rollout gate requires a Linux amd64 or arm64 Engine.",
            {"platform": value},
        )
    return normalized


def build_release_image(
    driver: Any,
    *,
    repository: str,
    slot: str,
    version: str,
    platform: str,
    owner: str,
    logs_dir: pathlib.Path,
    go_proxy: str | None,
) -> ReleaseImage:
    git_sha = driver._head_sha()
    tag = f"{repository}:{slot}-{git_sha[:12]}-{owner[:8]}"
    metadata_path = logs_dir / f"worker-{slot}-build-metadata.json"
    arguments = [
        "--target",
        "worker-acceptance",
        "--image",
        tag,
        "--version",
        version,
        "--git-sha",
        git_sha,
        "--source-date-epoch",
        driver._source_date_epoch(git_sha),
        "--platform",
        platform,
        "--metadata-file",
        str(metadata_path),
        "--label",
        f"{ROLLOUT_LABEL}=true",
        "--label",
        f"{OWNER_LABEL}={owner}",
        "--label",
        f"{SLOT_LABEL}={slot}",
        "--load",
    ]
    if go_proxy is not None:
        arguments.extend(["--go-proxy", go_proxy])
    driver._worker_build_command(
        arguments,
        log_path=logs_dir / f"worker-{slot}-build.log",
        maximum_timeout=max(120.0, driver.deadline.remaining()),
    )
    driver._docker_command(
        ["push", tag],
        log_path=logs_dir / f"worker-{slot}-push.log",
        maximum_timeout=max(120.0, driver.deadline.remaining()),
    )
    image_id = driver._docker_command(
        ["image", "inspect", "--format", "{{.Id}}", tag]
    ).strip()
    raw_repo_digests = driver._docker_command(
        ["image", "inspect", "--format", "{{json .RepoDigests}}", tag]
    ).strip()
    try:
        repo_digests = json.loads(raw_repo_digests)
    except json.JSONDecodeError:
        repo_digests = None
    matches = [
        item
        for item in repo_digests or []
        if isinstance(item, str) and item.startswith(repository + "@")
    ]
    if len(matches) != 1:
        raise acceptance.AcceptanceError(
            "runner.worker_release_registry_digest_missing",
            "The local Registry push did not return one exact repository digest.",
            {"slot": slot, "repoDigestCount": len(matches)},
        )
    exact_reference = matches[0]
    digest = exact_reference.rsplit("@", 1)[-1]
    if (
        re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None
        or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_registry_digest_invalid",
            "The pushed Worker image did not expose canonical SHA-256 identities.",
            {"slot": slot},
        )
    image = ReleaseImage(
        slot=slot,
        version=version,
        tag=tag,
        exact_reference=exact_reference,
        digest=digest,
        image_id=image_id,
        metadata_path=metadata_path,
    )
    validate_owned_release_image(driver, image, owner=owner)
    return image


def validate_owned_release_image(driver: Any, image: ReleaseImage, *, owner: str) -> bool:
    raw = driver._docker_command(
        ["image", "inspect", "--format", "{{json .Config.Labels}}", image.image_id]
    ).strip()
    try:
        labels = json.loads(raw)
    except json.JSONDecodeError:
        labels = None
    if (
        not isinstance(labels, dict)
        or labels.get(OWNER_LABEL) != owner
        or labels.get(SLOT_LABEL) != image.slot
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_image_not_owned",
            "The Worker rollout gate refused an image without its exact ownership labels.",
            {"slot": image.slot},
        )
    return True


def required_string(value: Mapping[str, Any], key: str, description: str) -> str:
    item = value.get(key)
    if not isinstance(item, str) or not item:
        raise acceptance.AcceptanceError(
            "runner.response_identity_missing",
            f"The {description} omitted {key}.",
        )
    return item


def release_base_path(tenant_id: str, target_id: str) -> str:
    return f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/worker-releases"


def release_action_path(
    tenant_id: str,
    target_id: str,
    revision_id: str,
    action: str,
) -> str:
    return f"{release_base_path(tenant_id, target_id)}/{revision_id}/{action}"


def release_image_evidence(image: ReleaseImage) -> dict[str, Any]:
    return {
        "slot": image.slot,
        "version": image.version,
        "tag": image.tag,
        "exactReference": image.exact_reference,
        "digest": image.digest,
        "imageId": image.image_id,
        "buildMetadata": str(image.metadata_path),
    }


def manifest_evidence(manifest: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "executionTargetId": manifest.get("executionTargetId"),
        "manifestId": manifest.get("manifestId"),
        "workerStatusCounts": manifest.get("workerStatusCounts"),
        "workerBuild": manifest.get("workerBuild"),
    }


def revision_evidence(revision: Mapping[str, Any]) -> dict[str, Any]:
    return {
        key: revision.get(key)
        for key in (
            "id",
            "revision",
            "workerManifestId",
            "workerBuildVersion",
            "workerBuildGitSha",
            "imageDigest",
            "description",
        )
    }


def policy_evidence(policy: Mapping[str, Any]) -> dict[str, Any]:
    return {
        key: policy.get(key)
        for key in (
            "policyVersion",
            "promotedRevisionId",
            "canaryRevisionId",
            "canaryPercent",
            "updatedAt",
        )
    }


def validate_managed_worker_binding(
    workers: Sequence[Mapping[str, Any]],
    *,
    execution: Mapping[str, Any],
    target_id: str,
    revision_id: str,
    channel: str,
    manifest_id: str,
) -> dict[str, Any]:
    worker_id = execution.get("workerId")
    if not isinstance(worker_id, str) or not worker_id:
        raise acceptance.AcceptanceError(
            "runner.worker_release_execution_worker_missing",
            "The release-pinned Execution omitted its Worker ID.",
        )
    matches = [worker for worker in workers if worker.get("id") == worker_id]
    if not matches:
        raise PendingReleaseEvidence()
    if len(matches) != 1:
        raise acceptance.AcceptanceError(
            "runner.worker_release_worker_identity_ambiguous",
            "The managed Worker API returned duplicate rows for one release-pinned Execution.",
            {"workerId": worker_id, "matchCount": len(matches)},
        )
    worker = matches[0]
    actual = {
        "executionTargetId": worker.get("executionTargetId"),
        "currentManifestId": worker.get("currentManifestId"),
        "workerReleaseRevisionId": worker.get("workerReleaseRevisionId"),
        "workerReleaseChannel": worker.get("workerReleaseChannel"),
        "workerReleaseStatus": worker.get("workerReleaseStatus"),
        "administrativeStatus": worker.get("administrativeStatus"),
        "status": worker.get("status"),
    }
    expected = {
        "executionTargetId": target_id,
        "currentManifestId": manifest_id,
        "workerReleaseRevisionId": revision_id,
        "workerReleaseChannel": channel,
        "workerReleaseStatus": "active",
        "administrativeStatus": "active",
        "status": "online",
    }
    pod_name = worker.get("podName")
    if actual != expected or not isinstance(pod_name, str) or not pod_name:
        raise acceptance.AcceptanceError(
            "runner.worker_release_worker_binding_mismatch",
            "The active Execution Worker did not retain its exact Manifest, Revision, Channel, and runtime identity.",
            {"workerId": worker_id, "expected": expected, "actual": actual},
        )
    return {
        "id": worker_id,
        "podName": pod_name,
        **actual,
    }


def validate_active_execution_conflict(
    conflict: Mapping[str, Any], *, revision_id: str, channel: str
) -> None:
    details = conflict.get("details")
    if (
        not isinstance(details, dict)
        or details.get("releaseRevisionId") != revision_id
        or details.get("releaseChannel") != channel
        or not isinstance(details.get("activeExecutions"), int)
        or details["activeExecutions"] < 1
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_active_execution_conflict_mismatch",
            "Worker Release promotion did not identify the exact busy release being retired.",
            {"expectedRevisionId": revision_id, "expectedChannel": channel},
        )


def validate_revision(
    revision: Mapping[str, Any],
    image: ReleaseImage,
    manifest_id: str,
) -> None:
    if (
        revision.get("workerManifestId") != manifest_id
        or revision.get("workerBuildVersion") != image.version
        or revision.get("imageDigest") != image.digest
        or not isinstance(revision.get("id"), str)
        or not isinstance(revision.get("revision"), int)
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_identity_mismatch",
            "An immutable Release Revision did not retain its Worker image identity.",
            {"slot": image.slot, "revision": revision_evidence(revision)},
        )


def validate_policy(
    policy: Mapping[str, Any],
    *,
    expected_version: int,
    promoted_id: str,
    canary_id: str | None,
    canary_percent: int,
) -> None:
    actual = {
        "policyVersion": policy.get("policyVersion"),
        "promotedRevisionId": policy.get("promotedRevisionId"),
        "canaryRevisionId": policy.get("canaryRevisionId"),
        "canaryPercent": policy.get("canaryPercent"),
    }
    expected = {
        "policyVersion": expected_version,
        "promotedRevisionId": promoted_id,
        "canaryRevisionId": canary_id,
        "canaryPercent": canary_percent,
    }
    if actual != expected:
        raise acceptance.AcceptanceError(
            "runner.worker_release_policy_mismatch",
            "Worker Release policy did not match the expected strict-CAS transition.",
            {"expected": expected, "actual": actual},
        )


def expect_problem(
    api: Any,
    method: str,
    path: str,
    payload: Mapping[str, Any],
    code: str,
) -> dict[str, Any]:
    try:
        api.request(method, path, payload)
    except acceptance.HTTPFailure as error:
        if error.code != code or error.evidence.get("status") != 409:
            raise acceptance.AcceptanceError(
                "runner.expected_problem_mismatch",
                "A Worker Release negative assertion returned the wrong Problem code.",
                {
                    "expectedCode": code,
                    "actualCode": error.code,
                    "actual": error.evidence,
                },
            ) from None
        problem = error.evidence.get("problem")
        return {
            "status": error.evidence.get("status"),
            "code": error.code,
            "details": problem.get("details") if isinstance(problem, dict) else None,
        }
    raise acceptance.AcceptanceError(
        "runner.expected_problem_missing",
        "A Worker Release negative assertion unexpectedly succeeded.",
        {"expectedCode": code, "path": path},
    )


def validate_execution_release_events(
    events: Sequence[Mapping[str, Any]],
    *,
    turn_id: str,
    revision_id: str,
    channel: str,
    manifest_id: str,
    terminal_required: bool,
) -> dict[str, Any]:
    created = next(
        (
            event
            for event in events
            if event.get("eventType") == "turn.created"
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("turnId") == turn_id
        ),
        None,
    )
    if created is None:
        raise PendingReleaseEvidence()
    execution_id = acceptance.AcceptanceSuite._event_execution_id(created)
    matching = [event for event in events if event.get("executionId") == execution_id]
    leased = next((event for event in matching if event.get("eventType") == "execution.leased"), None)
    if leased is None:
        raise PendingReleaseEvidence()
    for label, event in (("turn.created", created), ("execution.leased", leased)):
        payload = event.get("payload")
        if (
            not isinstance(payload, dict)
            or payload.get("workerReleaseRevisionId") != revision_id
            or payload.get("workerReleaseChannel") != channel
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_execution_pin_mismatch",
                f"{label} did not retain the expected Worker Release pin.",
                {"executionId": execution_id, "eventType": label},
            )
    leased_payload = leased["payload"]
    if leased_payload.get("workerManifestId") != manifest_id:
        raise acceptance.AcceptanceError(
            "runner.worker_release_execution_manifest_mismatch",
            "The leased Worker manifest did not match the selected Release Revision.",
            {"executionId": execution_id},
        )
    terminals = [
        event for event in matching if event.get("eventType") in acceptance.TERMINAL_EVENT_TYPES
    ]
    if len(terminals) > 1:
        raise acceptance.AcceptanceError(
            "runner.turn_terminal_duplicate",
            "A Worker Release Execution emitted more than one terminal event.",
            {"executionId": execution_id, "terminalCount": len(terminals)},
        )
    if terminal_required and len(terminals) != 1:
        raise PendingReleaseEvidence()
    terminal = terminals[0] if terminals else None
    return {
        "turnId": turn_id,
        "executionId": execution_id,
        "workerId": leased.get("workerId"),
        "generation": leased.get("generation"),
        "workerManifestId": manifest_id,
        "workerReleaseRevisionId": revision_id,
        "workerReleaseChannel": channel,
        "terminal": (
            acceptance.AcceptanceSuite._event_summary(terminal)
            if isinstance(terminal, Mapping)
            else None
        ),
        "terminalCount": len(terminals),
        "sequenceRange": acceptance.AcceptanceSuite._sequence_range(matching),
    }


def validate_release_load_identity(
    expected: Mapping[str, Any], release: Mapping[str, Any]
) -> None:
    expected_identity = {
        key: expected.get(key) for key in ("executionId", "workerId", "generation")
    }
    actual_identity = {
        key: release.get(key) for key in ("executionId", "workerId", "generation")
    }
    if (
        not isinstance(expected_identity["executionId"], str)
        or not expected_identity["executionId"]
        or not isinstance(expected_identity["workerId"], str)
        or not expected_identity["workerId"]
        or not isinstance(expected_identity["generation"], int)
        or actual_identity != expected_identity
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_load_identity_mismatch",
            "A bounded load Execution did not retain its release-pinned Worker identity.",
            {"expected": expected_identity, "actual": actual_identity},
        )


def validate_release_overview(
    overview: Mapping[str, Any],
    *,
    baseline_revision_id: str,
    candidate_revision_id: str,
    baseline_digest: str,
    candidate_digest: str,
) -> dict[str, Any]:
    policy = overview.get("policy")
    revisions = overview.get("revisions")
    transitions = overview.get("transitions")
    if not isinstance(policy, dict) or not isinstance(revisions, list) or not isinstance(transitions, list):
        raise acceptance.AcceptanceError(
            "runner.worker_release_overview_invalid",
            "Final Worker Release overview was malformed.",
        )
    validate_policy(
        policy,
        expected_version=4,
        promoted_id=baseline_revision_id,
        canary_id=None,
        canary_percent=0,
    )
    revision_by_id = {
        item.get("id"): item for item in revisions if isinstance(item, dict)
    }
    if set(revision_by_id) != {baseline_revision_id, candidate_revision_id}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_history_invalid",
            "Final Worker Release history did not retain exactly two immutable Revisions.",
        )
    if (
        revision_by_id[baseline_revision_id].get("imageDigest") != baseline_digest
        or revision_by_id[candidate_revision_id].get("imageDigest") != candidate_digest
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_history_invalid",
            "Final Worker Release history changed an immutable image digest.",
        )
    transition_by_version = {
        item.get("policyVersion"): item
        for item in transitions
        if isinstance(item, dict) and isinstance(item.get("policyVersion"), int)
    }
    if set(transition_by_version) != {1, 2, 3, 4}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_transition_history_invalid",
            "Final Worker Release history did not contain exactly four policy transitions.",
            {"policyVersions": sorted(transition_by_version)},
        )
    for version, action in EXPECTED_POLICY_ACTIONS:
        transition = transition_by_version[version]
        if transition.get("action") != action:
            raise acceptance.AcceptanceError(
                "runner.worker_release_transition_history_invalid",
                "A Worker Release transition changed action or order.",
                {"policyVersion": version, "action": transition.get("action")},
            )
    expected_targets = {
        1: (baseline_revision_id, None, 0),
        2: (baseline_revision_id, candidate_revision_id, 100),
        3: (candidate_revision_id, None, 0),
        4: (baseline_revision_id, None, 0),
    }
    for version, (promoted, canary, percent) in expected_targets.items():
        transition = transition_by_version[version]
        if (
            transition.get("toPromotedRevisionId") != promoted
            or transition.get("toCanaryRevisionId") != canary
            or transition.get("canaryPercent") != percent
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_transition_history_invalid",
                "A Worker Release transition changed its immutable target state.",
                {"policyVersion": version},
            )
    return {
        "policy": policy_evidence(policy),
        "revisionIds": sorted(revision_by_id),
        "transitionVersions": sorted(transition_by_version),
        "transitionActions": [transition_by_version[index]["action"] for index in range(1, 5)],
    }


def load_all_audit_logs(
    api: Any,
    tenant_id: str,
    *,
    page_limit: int = 200,
    maximum_pages: int = 100,
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    items: list[dict[str, Any]] = []
    seen_event_ids: set[str] = set()
    seen_cursors: set[str] = set()
    cursor: str | None = None
    page_count = 0
    while page_count < maximum_pages:
        path = f"/v1/tenants/{tenant_id}/audit-logs?limit={page_limit}"
        if cursor is not None:
            path += "&cursor=" + urllib.parse.quote(cursor, safe="")
        page = acceptance.json_object(
            api.request("GET", path),
            "Worker Release audit page",
        )
        raw_items = page.get("items")
        if not isinstance(raw_items, list) or not all(
            isinstance(item, dict) for item in raw_items
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_audit_invalid",
                "Worker Release audit API returned an invalid item list.",
            )
        for item in raw_items:
            event_id = item.get("eventId")
            if not isinstance(event_id, str) or not event_id or event_id in seen_event_ids:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_audit_pagination_invalid",
                    "Worker Release audit pagination omitted or repeated an Event identity.",
                    {"eventId": event_id, "page": page_count + 1},
                )
            seen_event_ids.add(event_id)
            items.append(dict(item))
        page_count += 1
        next_cursor = page.get("nextCursor")
        if next_cursor is None:
            return items, {"pageCount": page_count, "entryCount": len(items)}
        if (
            not isinstance(next_cursor, str)
            or not next_cursor
            or next_cursor in seen_cursors
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_audit_pagination_invalid",
                "Worker Release audit pagination returned an invalid or repeated cursor.",
                {"page": page_count},
            )
        seen_cursors.add(next_cursor)
        cursor = next_cursor
    raise acceptance.AcceptanceError(
        "runner.worker_release_audit_pagination_exhausted",
        "Worker Release audit pagination exceeded its bounded page count.",
        {"maximumPages": maximum_pages, "entryCount": len(items)},
    )


def validate_release_audit(
    items: Sequence[Mapping[str, Any]],
    *,
    target_id: str,
    revision_ids: set[str],
) -> dict[str, Any]:
    release_items = [item for item in items if str(item.get("action", "")).startswith("worker_release.")]
    revision_items = [item for item in release_items if item.get("action") == "worker_release.revision_created"]
    policy_items = [item for item in release_items if item.get("action") != "worker_release.revision_created"]
    if {item.get("resourceId") for item in revision_items} != revision_ids or len(revision_items) != 2:
        raise acceptance.AcceptanceError(
            "runner.worker_release_audit_invalid",
            "Worker Release audit did not retain exactly two Revision creation entries.",
        )
    by_version: dict[int, Mapping[str, Any]] = {}
    for item in policy_items:
        metadata = item.get("metadata")
        version = metadata.get("policyVersion") if isinstance(metadata, dict) else None
        if isinstance(version, int):
            by_version[version] = item
    if set(by_version) != {1, 2, 3, 4} or len(policy_items) != 4:
        raise acceptance.AcceptanceError(
            "runner.worker_release_audit_invalid",
            "Worker Release audit did not retain exactly four policy entries.",
            {"policyVersions": sorted(by_version)},
        )
    for version, action in EXPECTED_AUDIT_ACTIONS.items():
        item = by_version[version]
        if item.get("action") != action or item.get("resourceId") != target_id:
            raise acceptance.AcceptanceError(
                "runner.worker_release_audit_invalid",
                "A Worker Release audit entry did not match its target or policy action.",
                {"policyVersion": version},
            )
    return {
        "revisionEntryCount": len(revision_items),
        "policyEntryCount": len(policy_items),
        "policyActions": [by_version[index]["action"] for index in range(1, 5)],
    }


def validate_release_outbox(
    items: Sequence[Mapping[str, Any]],
    *,
    target_id: str,
    revision_ids: set[str],
) -> dict[str, Any]:
    release_items = [item for item in items if str(item.get("topic", "")).startswith("worker.release.")]
    expected = {
        ("worker.release.revision-created", revision_id) for revision_id in revision_ids
    }
    expected.update(
        (topic, f"{target_id}:{version}")
        for version, topic in EXPECTED_OUTBOX_TOPICS.items()
    )
    actual = {(item.get("topic"), item.get("messageKey")) for item in release_items}
    if actual != expected or len(release_items) != len(expected):
        raise acceptance.AcceptanceError(
            "runner.worker_release_outbox_invalid",
            "Worker Release Outbox did not retain exactly the immutable Revision and policy messages.",
            {"expectedCount": len(expected), "actualCount": len(release_items)},
        )
    invalid_statuses = [
        item.get("status")
        for item in release_items
        if item.get("status") not in {"pending", "retrying", "published"}
    ]
    if invalid_statuses:
        raise acceptance.AcceptanceError(
            "runner.worker_release_outbox_invalid",
            "Worker Release Outbox contained a dead-lettered or unknown status.",
            {"statuses": invalid_statuses},
        )
    return {
        "messageCount": len(release_items),
        "topics": sorted(str(item.get("topic")) for item in release_items),
        "statuses": sorted({str(item.get("status")) for item in release_items}),
    }


class WorkerReleaseAcceptanceSuite(acceptance.AcceptanceSuite):
    release_description_prefix = "Stage 3 Worker rollout"

    def __init__(
        self,
        options: acceptance.RunnerOptions,
        driver: Any,
        deadline: acceptance.Deadline,
        redactor: acceptance.SecretRedactor,
    ) -> None:
        super().__init__(options, driver, deadline, redactor)
        self.manifests: dict[str, dict[str, Any]] = {}
        self.revisions: dict[str, dict[str, Any]] = {}
        self.release_load_execution_ids: set[str] = set()
        self.release_load_phase_counts: dict[str, int] = {}

    def _wait_manifest(
        self,
        target_id: str,
        image: ReleaseImage,
        *,
        expected_online: int,
        manifest_id: str | None = None,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")

        def probe() -> dict[str, Any] | None:
            manifests = acceptance.json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/worker-manifests"),
                "worker manifests",
            )
            candidates = []
            for item in manifests:
                build = item.get("workerBuild")
                counts = item.get("workerStatusCounts")
                if (
                    item.get("executionTargetId") != target_id
                    or not isinstance(build, dict)
                    or not isinstance(counts, dict)
                    or build.get("imageDigest") != image.digest
                    or (manifest_id is not None and item.get("manifestId") != manifest_id)
                ):
                    continue
                candidates.append(item)
            if expected_online == 0:
                if any(int(item["workerStatusCounts"].get("online") or 0) != 0 for item in candidates):
                    return None
                return candidates[0] if candidates else {
                    "executionTargetId": target_id,
                    "manifestId": manifest_id,
                    "workerStatusCounts": {"online": 0, "draining": 0, "offline": 0},
                    "workerBuild": {
                        "version": image.version,
                        "gitSha": self.driver.head_sha,
                        "imageDigest": image.digest,
                    },
                }
            if len(candidates) != 1:
                return None
            item = candidates[0]
            counts = acceptance.json_object(
                item.get("workerStatusCounts"), "Worker manifest status counts"
            )
            build = acceptance.json_object(item.get("workerBuild"), "Worker manifest build")
            if int(counts.get("online") or 0) != expected_online:
                return None
            if build.get("version") != image.version or build.get("gitSha") != self.driver.head_sha:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_manifest_identity_mismatch",
                    "A Worker manifest did not retain the clean source identity.",
                    {"targetId": target_id, "slot": image.slot},
                )
            return item

        return self.api.wait_until(
            f"{image.slot} Worker manifest on Target {target_id}", probe, interval=0.25
        )

    def _create_release_revisions(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")
        base_path = release_base_path(tenant_id, target_id)
        for slot in ("baseline", "candidate"):
            manifest_id = required_string(
                self.manifests[slot], "manifestId", f"{slot} Worker manifest"
            )
            revision = acceptance.json_object(
                self.api.request(
                    "POST",
                    base_path,
                    {
                        "workerManifestId": manifest_id,
                        "description": f"{self.release_description_prefix} {slot}",
                    },
                    expected=(201,),
                ),
                f"{slot} Release Revision",
            )
            validate_revision(revision, self.driver.images[slot], manifest_id)
            self.revisions[slot] = revision
        duplicate = expect_problem(
            self.api,
            "POST",
            base_path,
            {
                "workerManifestId": self.manifests["baseline"]["manifestId"],
                "description": "duplicate immutable registration must fail",
            },
            "worker_release_manifest_already_registered",
        )
        overview = acceptance.json_object(
            self.api.request("GET", base_path), "Worker Release overview"
        )
        if len(overview.get("revisions", [])) != 2:
            raise acceptance.AcceptanceError(
                "runner.worker_release_revision_count_invalid",
                "Duplicate Release registration changed the immutable Revision set.",
            )
        return {
            "baseline": revision_evidence(self.revisions["baseline"]),
            "candidate": revision_evidence(self.revisions["candidate"]),
            "duplicateRegistration": duplicate,
        }

    def _wait_managed_worker(
        self,
        execution: Mapping[str, Any],
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")

        def probe() -> dict[str, Any] | None:
            workers = acceptance.json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/workers"),
                "managed Workers",
            )
            try:
                return validate_managed_worker_binding(
                    workers,
                    execution=execution,
                    target_id=target_id,
                    revision_id=revision_id,
                    channel=channel,
                    manifest_id=manifest_id,
                )
            except PendingReleaseEvidence:
                return None

        return self.api.wait_until(
            f"managed Worker binding for Execution {execution.get('executionId')}",
            probe,
            interval=0.2,
        )

    def _wait_execution_release(
        self,
        turn_id: str,
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
        terminal: bool,
        session_id: str | None = None,
    ) -> Mapping[str, Any]:
        def probe() -> Mapping[str, Any] | None:
            events = (
                self._all_events()
                if session_id is None or session_id == self.state.session_id
                else self._all_events(session_id=session_id)
            )
            try:
                return validate_execution_release_events(
                    events,
                    turn_id=turn_id,
                    revision_id=revision_id,
                    channel=channel,
                    manifest_id=manifest_id,
                    terminal_required=terminal,
                )
            except PendingReleaseEvidence:
                return None

        return self.api.wait_until(
            f"release-pinned Execution for Turn {turn_id}", probe, interval=0.2
        )

    def _policy_change(
        self,
        action: str,
        slot: str,
        *,
        expected_version: int,
        reason: str,
        canary_percent: int = 0,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "expectedPolicyVersion": expected_version,
            "reason": reason,
        }
        if action == "canary":
            payload["canaryPercent"] = canary_percent
        return acceptance.json_object(
            self.api.request(
                "POST",
                release_action_path(
                    self._required("tenant_id"),
                    self._required("target_id"),
                    self.revisions[slot]["id"],
                    action,
                ),
                payload,
            ),
            f"Worker Release {action}",
        )

    def _release_load_active_runtime(
        self,
        *,
        slot: str,
        channel: str,
        load_turn: Mapping[str, Any],
        release: Mapping[str, Any],
        worker: Mapping[str, Any],
    ) -> Mapping[str, Any] | None:
        del slot, channel, load_turn, release, worker
        return None

    def _release_load_waves(
        self,
        *,
        slot: str,
        channel: str,
        wave_start: int,
        wave_count: int,
        require_runtime_evidence: bool = False,
        expected_distinct_workers: int = acceptance.FIXTURE_CONCURRENCY_WORKERS,
    ) -> Mapping[str, Any]:
        revision_id = required_string(
            self.revisions[slot], "id", f"{slot} Release Revision"
        )
        manifest_id = required_string(
            self.manifests[slot], "manifestId", f"{slot} Worker Manifest"
        )
        phase = f"{slot}-{channel}"
        active_checks = 0
        terminal_checks = 0
        worker_binding_checks = 0
        runtime_checks = 0
        samples: list[dict[str, Any]] = []

        def active_validator(load_turn: Mapping[str, Any]) -> None:
            nonlocal active_checks, worker_binding_checks, runtime_checks
            turn = acceptance.json_object(load_turn.get("turn"), "release load Turn")
            active = acceptance.json_object(
                load_turn.get("active"), "release load active Execution"
            )
            turn_id = self._turn_id(turn, "release load Turn")
            release = self._wait_execution_release(
                turn_id,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
                terminal=False,
                session_id=str(load_turn["sessionId"]),
            )
            validate_release_load_identity(active, release)
            worker = self._wait_managed_worker(
                release,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
            )
            runtime = self._release_load_active_runtime(
                slot=slot,
                channel=channel,
                load_turn=load_turn,
                release=release,
                worker=worker,
            )
            active_checks += 1
            worker_binding_checks += 1
            if runtime is not None:
                runtime_checks += 1
            if len(samples) < 2:
                sample: dict[str, Any] = {
                    "stage": "active",
                    "sessionId": load_turn.get("sessionId"),
                    "execution": release,
                    "worker": worker,
                }
                if runtime is not None:
                    sample["runtime"] = dict(runtime)
                samples.append(sample)

        def terminal_validator(
            load_turn: Mapping[str, Any], terminal: Mapping[str, Any]
        ) -> None:
            nonlocal terminal_checks
            turn = acceptance.json_object(load_turn.get("turn"), "release load Turn")
            turn_id = self._turn_id(turn, "release load Turn")
            release = self._wait_execution_release(
                turn_id,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
                terminal=True,
                session_id=str(load_turn["sessionId"]),
            )
            validate_release_load_identity(terminal, release)
            execution_id = required_string(
                release, "executionId", "release load Execution"
            )
            if execution_id in self.release_load_execution_ids:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_load_execution_reused",
                    "Worker Release load reused an Execution across rollout phases.",
                    {"phase": phase, "executionId": execution_id},
                )
            self.release_load_execution_ids.add(execution_id)
            terminal_checks += 1
            if len(samples) < 4:
                samples.append(
                    {
                        "stage": "terminal",
                        "sessionId": load_turn.get("sessionId"),
                        "execution": release,
                    }
                )

        load = self._fixture_load_admission_waves(
            wave_start=wave_start,
            wave_count=wave_count,
            expected_distinct_workers=expected_distinct_workers,
            active_validator=active_validator,
            terminal_validator=terminal_validator,
        )
        expected_executions = wave_count * acceptance.FIXTURE_LOAD_SESSIONS
        if (
            active_checks != expected_executions
            or terminal_checks != expected_executions
            or worker_binding_checks != expected_executions
            or (require_runtime_evidence and runtime_checks != expected_executions)
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_load_pin_count_invalid",
                "Worker Release load did not validate every active and terminal release pin.",
                {
                    "phase": phase,
                    "expectedExecutions": expected_executions,
                    "activeChecks": active_checks,
                    "terminalChecks": terminal_checks,
                    "workerBindingChecks": worker_binding_checks,
                    "runtimeChecks": runtime_checks,
                },
            )
        self.release_load_phase_counts[phase] = expected_executions
        return {
            **dict(load),
            "phase": phase,
            "revisionId": revision_id,
            "channel": channel,
            "manifestId": manifest_id,
            "registryDigest": self.driver.images[slot].digest,
            "activeReleasePinChecks": active_checks,
            "terminalReleasePinChecks": terminal_checks,
            "workerBindingChecks": worker_binding_checks,
            "activeRuntimeChecks": runtime_checks,
            "releasePinSamples": samples,
        }
