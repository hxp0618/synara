from __future__ import annotations

import contextlib
import io
import pathlib
import tempfile
import unittest
from collections.abc import Mapping

import acceptance_runner as acceptance
import docker_worker_release_rollout_gate as gate


BASELINE_REVISION = "11111111-1111-4111-8111-111111111111"
CANDIDATE_REVISION = "22222222-2222-4222-8222-222222222222"
BASELINE_MANIFEST = "33333333-3333-4333-8333-333333333333"
CANDIDATE_MANIFEST = "44444444-4444-4444-8444-444444444444"
TARGET_ID = "55555555-5555-4555-8555-555555555555"
WORKER_ID = "66666666-6666-4666-8666-666666666666"
BASELINE_DIGEST = "sha256:" + "a" * 64
CANDIDATE_DIGEST = "sha256:" + "b" * 64


def sample_overview() -> dict[str, object]:
    return {
        "policy": {
            "policyVersion": 4,
            "promotedRevisionId": BASELINE_REVISION,
            "canaryRevisionId": None,
            "canaryPercent": 0,
        },
        "revisions": [
            {"id": CANDIDATE_REVISION, "imageDigest": CANDIDATE_DIGEST},
            {"id": BASELINE_REVISION, "imageDigest": BASELINE_DIGEST},
        ],
        "transitions": [
            {
                "policyVersion": 4,
                "action": "rollback",
                "toPromotedRevisionId": BASELINE_REVISION,
                "toCanaryRevisionId": None,
                "canaryPercent": 0,
            },
            {
                "policyVersion": 3,
                "action": "promote",
                "toPromotedRevisionId": CANDIDATE_REVISION,
                "toCanaryRevisionId": None,
                "canaryPercent": 0,
            },
            {
                "policyVersion": 2,
                "action": "canary",
                "toPromotedRevisionId": BASELINE_REVISION,
                "toCanaryRevisionId": CANDIDATE_REVISION,
                "canaryPercent": 100,
            },
            {
                "policyVersion": 1,
                "action": "promote",
                "toPromotedRevisionId": BASELINE_REVISION,
                "toCanaryRevisionId": None,
                "canaryPercent": 0,
            },
        ],
    }


def sample_events(*, terminal_count: int = 1) -> list[dict[str, object]]:
    events: list[dict[str, object]] = [
        {
            "sequence": 1,
            "eventType": "turn.created",
            "executionId": "execution-1",
            "payload": {
                "turnId": "turn-1",
                "executionId": "execution-1",
                "workerReleaseRevisionId": CANDIDATE_REVISION,
                "workerReleaseChannel": "canary",
            },
        },
        {
            "sequence": 2,
            "eventType": "execution.leased",
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
            "payload": {
                "workerManifestId": CANDIDATE_MANIFEST,
                "workerReleaseRevisionId": CANDIDATE_REVISION,
                "workerReleaseChannel": "canary",
            },
        },
    ]
    for index in range(terminal_count):
        events.append(
            {
                "sequence": index + 3,
                "eventType": "execution.completed",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {},
            }
        )
    return events


def sample_audit() -> list[dict[str, object]]:
    items: list[dict[str, object]] = [
        {
            "action": "worker_release.revision_created",
            "resourceId": BASELINE_REVISION,
            "metadata": {},
        },
        {
            "action": "worker_release.revision_created",
            "resourceId": CANDIDATE_REVISION,
            "metadata": {},
        },
    ]
    for version, action in gate.EXPECTED_AUDIT_ACTIONS.items():
        items.append(
            {
                "action": action,
                "resourceId": TARGET_ID,
                "metadata": {"policyVersion": version},
            }
        )
    return items


def sample_outbox() -> list[dict[str, object]]:
    items: list[dict[str, object]] = [
        {
            "topic": "worker.release.revision-created",
            "messageKey": BASELINE_REVISION,
            "status": "published",
        },
        {
            "topic": "worker.release.revision-created",
            "messageKey": CANDIDATE_REVISION,
            "status": "published",
        },
    ]
    for version, topic in gate.EXPECTED_OUTBOX_TOPICS.items():
        items.append(
            {
                "topic": topic,
                "messageKey": f"{TARGET_ID}:{version}",
                "status": "published",
            }
        )
    return items


class ParseArgsTest(unittest.TestCase):
    def test_builds_isolated_defaults_without_credentials(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate.parse_args(["--output-dir", directory, "--go-proxy", "https://goproxy.cn,direct"])

        self.assertEqual(options.registry_image, gate.DEFAULT_REGISTRY_IMAGE)
        self.assertEqual(options.go_proxy, "https://goproxy.cn,direct")
        self.assertEqual(options.load_waves, 25)
        self.assertFalse(options.skip_build)

    def test_rejects_unsafe_build_and_proxy_inputs(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            gate.parse_args(["--skip-build"])
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            gate.parse_args(["--go-proxy", "https://user:secret@example.test"])
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            gate.parse_args(["--registry-image", "https://registry.invalid/image"])
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            gate.parse_args(["--load-waves", "1"])

    def test_passes_rollout_load_configuration_to_shared_runner(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate.parse_args(
                ["--output-dir", directory, "--load-waves", "2"]
            )

        child = gate.runner_options(options)

        self.assertEqual(child.suite, "fixture-load")
        self.assertEqual(child.load_waves, 2)


class IdentityHelperTest(unittest.TestCase):
    def test_rollout_versions_remain_distinct_and_valid(self) -> None:
        self.assertEqual(gate.rollout_version("0.5.4", "baseline"), "0.5.4+rollout.baseline")
        self.assertEqual(
            gate.rollout_version("0.5.4+build", "candidate"),
            "0.5.4+build.rollout.candidate",
        )

    def test_normalizes_only_supported_engine_platforms(self) -> None:
        self.assertEqual(gate.normalize_engine_platform("linux/aarch64"), "linux/arm64")
        self.assertEqual(gate.normalize_engine_platform("linux/x86_64"), "linux/amd64")
        with self.assertRaises(acceptance.AcceptanceError):
            gate.normalize_engine_platform("windows/amd64")

    def test_counts_pool_by_channel_revision_and_digest(self) -> None:
        counts = gate.container_pool_counts(
            [
                {"channel": "promoted", "revisionId": BASELINE_REVISION, "digest": BASELINE_DIGEST},
                {"channel": "canary", "revisionId": CANDIDATE_REVISION, "digest": CANDIDATE_DIGEST},
                {"channel": "canary", "revisionId": CANDIDATE_REVISION, "digest": CANDIDATE_DIGEST},
            ]
        )
        self.assertEqual(counts[("promoted", BASELINE_REVISION, BASELINE_DIGEST)], 1)
        self.assertEqual(counts[("canary", CANDIDATE_REVISION, CANDIDATE_DIGEST)], 2)

    def test_treats_stopped_transition_container_as_pending(self) -> None:
        self.assertTrue(gate.container_pool_running([{"State": {"Running": True}}]))
        self.assertFalse(
            gate.container_pool_running(
                [{"State": {"Running": True}}, {"State": {"Running": False}}]
            )
        )

    def test_treats_reconciler_delete_between_list_and_inspect_as_pending(self) -> None:
        self.assertTrue(gate.docker_container_missing("Error: No such object: abc"))
        self.assertTrue(gate.docker_container_missing("No such container: abc"))
        self.assertFalse(gate.docker_container_missing("permission denied"))


class ExecutionReleaseValidationTest(unittest.TestCase):
    def test_requires_matching_created_leased_and_single_terminal_events(self) -> None:
        evidence = gate.validate_execution_release_events(
            sample_events(),
            turn_id="turn-1",
            revision_id=CANDIDATE_REVISION,
            channel="canary",
            manifest_id=CANDIDATE_MANIFEST,
            terminal_required=True,
        )

        self.assertEqual(evidence["executionId"], "execution-1")
        self.assertEqual(evidence["terminalCount"], 1)
        self.assertEqual(evidence["workerReleaseChannel"], "canary")

    def test_waits_for_terminal_without_converting_pending_to_pass(self) -> None:
        with self.assertRaises(gate.PendingReleaseEvidence):
            gate.validate_execution_release_events(
                sample_events(terminal_count=0),
                turn_id="turn-1",
                revision_id=CANDIDATE_REVISION,
                channel="canary",
                manifest_id=CANDIDATE_MANIFEST,
                terminal_required=True,
            )

    def test_rejects_wrong_release_pin_or_duplicate_terminal(self) -> None:
        wrong = sample_events()
        leased = wrong[1]["payload"]
        assert isinstance(leased, dict)
        leased["workerReleaseChannel"] = "promoted"
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_execution_release_events(
                wrong,
                turn_id="turn-1",
                revision_id=CANDIDATE_REVISION,
                channel="canary",
                manifest_id=CANDIDATE_MANIFEST,
                terminal_required=True,
            )
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_execution_release_events(
                sample_events(terminal_count=2),
                turn_id="turn-1",
                revision_id=CANDIDATE_REVISION,
                channel="canary",
                manifest_id=CANDIDATE_MANIFEST,
                terminal_required=True,
            )

    def test_waits_on_the_explicit_non_primary_session(self) -> None:
        class API:
            @staticmethod
            def wait_until(
                _description: str,
                probe: object,
                *,
                interval: float,
            ) -> object:
                self.assertEqual(interval, 0.2)
                assert callable(probe)
                return probe()

        class Suite:
            def __init__(self) -> None:
                self.api = API()
                self.state = acceptance.ScenarioState(session_id="primary-session")
                self.requested_sessions: list[str | None] = []

            def _all_events(
                self, *, session_id: str | None = None
            ) -> list[dict[str, object]]:
                self.requested_sessions.append(session_id)
                return sample_events()

        suite = Suite()
        evidence = gate.WorkerReleaseRolloutSuite._wait_execution_release(
            suite,  # type: ignore[arg-type]
            "turn-1",
            revision_id=CANDIDATE_REVISION,
            channel="canary",
            manifest_id=CANDIDATE_MANIFEST,
            terminal=True,
            session_id="candidate-session",
        )

        self.assertEqual(evidence["executionId"], "execution-1")
        self.assertEqual(suite.requested_sessions, ["candidate-session"])


class BusyWorkerValidationTest(unittest.TestCase):
    def test_maps_execution_worker_to_exact_release_container_and_preserves_identity(self) -> None:
        execution = {"workerId": WORKER_ID, "executionId": "execution-1"}
        worker = gate.validate_managed_worker_binding(
            [
                {
                    "id": WORKER_ID,
                    "podName": "synara-agentd-target-promoted-0",
                    "executionTargetId": TARGET_ID,
                    "currentManifestId": BASELINE_MANIFEST,
                    "workerReleaseRevisionId": BASELINE_REVISION,
                    "workerReleaseChannel": "promoted",
                    "workerReleaseStatus": "active",
                    "administrativeStatus": "active",
                    "status": "online",
                }
            ],
            execution=execution,
            target_id=TARGET_ID,
            revision_id=BASELINE_REVISION,
            channel="promoted",
            manifest_id=BASELINE_MANIFEST,
        )
        container = {
            "id": "container123",
            "name": worker["podName"],
            "imageId": "sha256:image",
            "digest": BASELINE_DIGEST,
            "revisionId": BASELINE_REVISION,
            "channel": "promoted",
            "volume": "rollout-volume",
        }
        selected = gate.pool_container_for_worker({"containers": [container]}, worker)
        evidence = gate.validate_busy_container_preserved(container, selected)

        self.assertEqual(selected["id"], "container123")
        self.assertTrue(evidence["preservedWhileBusy"])

    def test_rejects_worker_binding_replacement_and_wrong_conflict_release(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_managed_worker_binding(
                [
                    {
                        "id": WORKER_ID,
                        "podName": "worker",
                        "executionTargetId": TARGET_ID,
                        "currentManifestId": BASELINE_MANIFEST,
                        "workerReleaseRevisionId": CANDIDATE_REVISION,
                        "workerReleaseChannel": "canary",
                        "workerReleaseStatus": "active",
                        "administrativeStatus": "active",
                        "status": "online",
                    }
                ],
                execution={"workerId": WORKER_ID},
                target_id=TARGET_ID,
                revision_id=BASELINE_REVISION,
                channel="promoted",
                manifest_id=BASELINE_MANIFEST,
            )
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_busy_container_preserved(
                {"id": "before", "name": "worker"},
                {"id": "after", "name": "worker"},
            )
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_active_execution_conflict(
                {
                    "details": {
                        "releaseRevisionId": CANDIDATE_REVISION,
                        "releaseChannel": "canary",
                        "activeExecutions": 1,
                    }
                },
                revision_id=BASELINE_REVISION,
                channel="promoted",
            )

    def test_accepts_exact_busy_release_conflict(self) -> None:
        gate.validate_active_execution_conflict(
            {
                "details": {
                    "releaseRevisionId": BASELINE_REVISION,
                    "releaseChannel": "promoted",
                    "activeExecutions": 1,
                }
            },
            revision_id=BASELINE_REVISION,
            channel="promoted",
        )

    def test_validates_release_load_and_container_recovery_identity(self) -> None:
        active = {
            "executionId": "execution-1",
            "workerId": WORKER_ID,
            "generation": 1,
            "requestId": "request-1",
            "interactionId": "interaction-1",
        }
        gate.validate_release_load_identity(active, dict(active))
        image = gate.ReleaseImage(
            slot="candidate",
            version="0.5.4+rollout.candidate",
            tag="candidate:tag",
            exact_reference=f"candidate@{CANDIDATE_DIGEST}",
            digest=CANDIDATE_DIGEST,
            image_id="sha256:" + "c" * 64,
            metadata_path=pathlib.Path("metadata.json"),
        )
        before = {
            "id": "before123456",
            "name": "candidate-worker",
            "imageId": image.image_id,
            "digest": CANDIDATE_DIGEST,
            "revisionId": CANDIDATE_REVISION,
            "channel": "canary",
            "volume": "rollout-volume",
        }
        after = {**before, "id": "after1234567"}
        target_recovery = {
            "fault": "worker-container-loss",
            "executionId": "execution-1",
            "executionGeneration": 1,
            "workerId": WORKER_ID,
            "removedContainerId": "before123456",
            "replacementContainerId": "after1234567",
            "containerName": "candidate-worker",
            "containerIdChanged": True,
            "exactExecutionWorkerMatch": True,
            "workerIdStable": True,
            "workerIncarnationAdvanced": True,
            "instanceUidChanged": True,
            "replacementReady": True,
            "namedVolumeContinuity": {"preservedAcrossReplacement": True},
        }
        recovery = {
            "staleExecutionId": "execution-1",
            "replacementExecutionId": "execution-1",
            "staleWorkerId": WORKER_ID,
            "replacementWorkerId": WORKER_ID,
            "staleGeneration": 1,
            "replacementGeneration": 2,
            "staleInteractionId": "interaction-1",
            "replacementInteractionId": "interaction-2",
            "staleRequestId": "request-1",
            "replacementRequestId": "request-2",
        }

        evidence = gate.validate_release_container_loss_recovery(
            before,
            after,
            active=active,
            recovery=recovery,
            target_recovery=target_recovery,
            revision_id=CANDIDATE_REVISION,
            channel="canary",
            image=image,
        )

        self.assertEqual(evidence["generation"], {"before": 1, "after": 2})
        self.assertTrue(evidence["workerIdStable"])

    def test_rejects_release_rebind_or_peer_generation_change(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_load_identity(
                {
                    "executionId": "execution-1",
                    "workerId": WORKER_ID,
                    "generation": 1,
                },
                {
                    "executionId": "execution-1",
                    "workerId": WORKER_ID,
                    "generation": 2,
                },
            )
        before_active = {
            "executionId": "baseline-execution",
            "workerId": WORKER_ID,
            "generation": 1,
            "requestId": "request-1",
            "interactionId": "interaction-1",
        }
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_peer_preserved(
                before_active,
                {**before_active, "generation": 2},
                {"id": "container", "name": "baseline-worker"},
                {"id": "container", "name": "baseline-worker"},
            )


class OverviewValidationTest(unittest.TestCase):
    def test_accepts_exact_four_transition_history(self) -> None:
        evidence = gate.validate_release_overview(
            sample_overview(),
            baseline_revision_id=BASELINE_REVISION,
            candidate_revision_id=CANDIDATE_REVISION,
            baseline_digest=BASELINE_DIGEST,
            candidate_digest=CANDIDATE_DIGEST,
        )

        self.assertEqual(evidence["transitionVersions"], [1, 2, 3, 4])
        self.assertEqual(evidence["transitionActions"], ["promote", "canary", "promote", "rollback"])

    def test_rejects_digest_mutation_or_transition_reordering(self) -> None:
        mutated = sample_overview()
        revisions = mutated["revisions"]
        assert isinstance(revisions, list)
        candidate = revisions[0]
        assert isinstance(candidate, dict)
        candidate["imageDigest"] = BASELINE_DIGEST
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_overview(
                mutated,
                baseline_revision_id=BASELINE_REVISION,
                candidate_revision_id=CANDIDATE_REVISION,
                baseline_digest=BASELINE_DIGEST,
                candidate_digest=CANDIDATE_DIGEST,
            )
        reordered = sample_overview()
        transitions = reordered["transitions"]
        assert isinstance(transitions, list)
        first = transitions[0]
        assert isinstance(first, dict)
        first["action"] = "promote"
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_overview(
                reordered,
                baseline_revision_id=BASELINE_REVISION,
                candidate_revision_id=CANDIDATE_REVISION,
                baseline_digest=BASELINE_DIGEST,
                candidate_digest=CANDIDATE_DIGEST,
            )


class DurableSideEffectValidationTest(unittest.TestCase):
    class PagedAuditAPI:
        def __init__(self) -> None:
            self.paths: list[str] = []

        def request(self, method: str, path: str) -> object:
            self.paths.append(path)
            if len(self.paths) == 1:
                return {
                    "items": [
                        {
                            "eventId": "noise-event",
                            "action": "execution.completed",
                        }
                    ],
                    "nextCursor": "cursor+/=",
                }
            return {
                "items": [
                    {**item, "eventId": f"release-event-{index}"}
                    for index, item in enumerate(sample_audit(), start=1)
                ],
                "nextCursor": None,
            }

    def test_accepts_exact_audit_and_outbox_sets(self) -> None:
        audit = gate.validate_release_audit(
            sample_audit(), target_id=TARGET_ID, revision_ids={BASELINE_REVISION, CANDIDATE_REVISION}
        )
        outbox = gate.validate_release_outbox(
            sample_outbox(), target_id=TARGET_ID, revision_ids={BASELINE_REVISION, CANDIDATE_REVISION}
        )

        self.assertEqual(audit["revisionEntryCount"], 2)
        self.assertEqual(audit["policyEntryCount"], 4)
        self.assertEqual(outbox["messageCount"], 6)

    def test_paginates_past_load_audits_to_release_history(self) -> None:
        api = self.PagedAuditAPI()

        items, pagination = gate.load_all_audit_logs(api, TARGET_ID)
        audit = gate.validate_release_audit(
            items,
            target_id=TARGET_ID,
            revision_ids={BASELINE_REVISION, CANDIDATE_REVISION},
        )

        self.assertEqual(pagination, {"pageCount": 2, "entryCount": 7})
        self.assertEqual(audit["revisionEntryCount"], 2)
        self.assertIn("cursor=cursor%2B%2F%3D", api.paths[1])

    def test_rejects_extra_audit_or_dead_lettered_outbox(self) -> None:
        audits = sample_audit()
        audits.append(dict(audits[-1]))
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_audit(
                audits, target_id=TARGET_ID, revision_ids={BASELINE_REVISION, CANDIDATE_REVISION}
            )
        outbox = sample_outbox()
        outbox[-1]["status"] = "dead-letter"
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_outbox(
                outbox, target_id=TARGET_ID, revision_ids={BASELINE_REVISION, CANDIDATE_REVISION}
            )


class ProblemAssertionTest(unittest.TestCase):
    class FailingAPI:
        def request(self, method: str, path: str, payload: Mapping[str, object]) -> object:
            raise acceptance.HTTPFailure(
                method,
                path,
                409,
                '{"error":{"code":"worker_release_policy_version_conflict",'
                '"message":"conflict","details":{"currentPolicyVersion":2}}}',
            )

    class PassingAPI:
        def request(self, _method: str, _path: str, _payload: Mapping[str, object]) -> object:
            return {}

    def test_requires_the_exact_problem_code(self) -> None:
        evidence = gate.expect_problem(
            self.FailingAPI(),
            "POST",
            "/release",
            {},
            "worker_release_policy_version_conflict",
        )
        self.assertEqual(evidence["status"], 409)
        self.assertEqual(evidence["details"], {"currentPolicyVersion": 2})
        with self.assertRaises(acceptance.AcceptanceError):
            gate.expect_problem(
                self.PassingAPI(),
                "POST",
                "/release",
                {},
                "worker_release_policy_version_conflict",
            )


if __name__ == "__main__":
    unittest.main()
