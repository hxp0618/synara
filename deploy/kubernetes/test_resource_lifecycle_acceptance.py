#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import ModuleType
from unittest import mock


SCRIPT = Path(__file__).with_name("resource-lifecycle-acceptance.py")


def load_module() -> ModuleType:
    spec = importlib.util.spec_from_file_location("resource_lifecycle_acceptance", SCRIPT)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load resource lifecycle acceptance module")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


acceptance = load_module()


class RecoveryBundleValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.execution_id = "11111111-1111-4111-8111-111111111111"
        self.session_id = "22222222-2222-4222-8222-222222222222"
        self.request_id = "fixture-approval-generation-1-1"
        self.target_id = "55555555-5555-4555-8555-555555555555"
        self.first_id = "33333333-3333-4333-8333-333333333333"
        self.bundles = [
            {
                "id": self.first_id,
                "generation": 1,
                "schemaVersion": 1,
                "recoveryReason": "initial-claim",
                "previousBundleId": None,
                "payloadSha256": "a" * 64,
                "payload": {},
            },
            {
                "id": "44444444-4444-4444-8444-444444444444",
                "generation": 2,
                "schemaVersion": 1,
                "recoveryReason": "suspend-resume",
                "previousBundleId": self.first_id,
                "payloadSha256": "b" * 64,
                "payload": {
                    "schemaVersion": 1,
                    "executionId": self.execution_id,
                    "sessionId": self.session_id,
                    "generation": 2,
                    "recoveryReason": "suspend-resume",
                    "execution": {
                        "targetKind": "kubernetes",
                        "executionTargetId": self.target_id,
                        "schedulingDecisionId": "66666666-6666-4666-8666-666666666666",
                    },
                    "workload": {
                        "provider": "codex",
                        "inputText": "[workspace-verify] [approval]",
                        "memoryReferences": [],
                        "resumeSnapshot": {
                            "pendingInteractions": [],
                            "resumeRecordedInteractions": [
                                {
                                    "kind": "approval",
                                    "requestId": self.request_id,
                                    "resolution": {"decision": "accept"},
                                }
                            ],
                            "workspace": {
                                "checkpoint": {
                                    "checkpointId": "77777777-7777-4777-8777-777777777777"
                                }
                            },
                        },
                    },
                },
            },
        ]

    def test_accepts_exact_suspend_resume_lineage_and_coverage(self) -> None:
        result = acceptance.validate_recovery_bundles(
            self.bundles, self.execution_id, self.session_id, self.request_id, self.target_id
        )
        self.assertEqual(result["bundleCount"], 2)
        self.assertTrue(result["lineageVerified"])
        self.assertTrue(all(result["coverage"].values()))

    def test_rejects_missing_workspace_or_changed_resolution(self) -> None:
        del self.bundles[1]["payload"]["workload"]["resumeSnapshot"]["workspace"]
        with self.assertRaisesRegex(acceptance.AcceptanceError, "omitted required"):
            acceptance.validate_recovery_bundles(
                self.bundles, self.execution_id, self.session_id, self.request_id, self.target_id
            )

    def test_rejects_non_linear_or_extra_bundles(self) -> None:
        self.bundles[1]["previousBundleId"] = "88888888-8888-4888-8888-888888888888"
        with self.assertRaisesRegex(acceptance.AcceptanceError, "lineage"):
            acceptance.validate_recovery_bundles(
                self.bundles, self.execution_id, self.session_id, self.request_id, self.target_id
            )
        with self.assertRaisesRegex(acceptance.AcceptanceError, "exactly"):
            acceptance.validate_recovery_bundles(
                self.bundles + [self.bundles[1]],
                self.execution_id,
                self.session_id,
                self.request_id,
                self.target_id,
            )

    def test_rejects_wrong_execution_target(self) -> None:
        with self.assertRaisesRegex(acceptance.AcceptanceError, "omitted required"):
            acceptance.validate_recovery_bundles(
                self.bundles,
                self.execution_id,
                self.session_id,
                self.request_id,
                "99999999-9999-4999-8999-999999999999",
            )


class ProblemParsingTest(unittest.TestCase):
    def test_reads_control_plane_problem_envelope(self) -> None:
        payload = json.dumps({"error": {"code": "session_execution_active"}}).encode()
        self.assertEqual(acceptance.parse_problem_code(payload), "session_execution_active")

    def test_rejects_malformed_or_non_string_problem_code(self) -> None:
        self.assertIsNone(acceptance.parse_problem_code(b"not-json"))
        self.assertIsNone(acceptance.parse_problem_code(b'{"error":{"code":409}}'))


class CleanupSafetyTest(unittest.TestCase):
    class FakeKubectl:
        def __init__(self, namespace: dict | None):
            self.value = namespace
            self.deletes = 0

        def namespace(self, _name: str, *, required: bool = True):
            del required
            return self.value

        def run(self, arguments, *, input_bytes=None):
            if arguments[:2] != ["delete", "--raw"] or not input_bytes:
                raise AssertionError("unexpected delete call")
            self.deletes += 1
            self.value = None

    def test_cleanup_requires_exact_target_label_and_uid(self) -> None:
        fake = self.FakeKubectl(
            {
                "metadata": {
                    "uid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
                    "labels": {acceptance.TARGET_LABEL: "wrong-target"},
                }
            }
        )
        with self.assertRaisesRegex(acceptance.AcceptanceError, "Refusing"):
            acceptance.delete_owned_worker_namespace(
                fake,
                "synara-lifecycle-test",
                "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
                1,
            )
        self.assertEqual(fake.deletes, 0)

    def test_cleanup_uses_uid_precondition_and_waits_for_absence(self) -> None:
        target_id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
        fake = self.FakeKubectl(
            {
                "metadata": {
                    "uid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
                    "labels": {acceptance.TARGET_LABEL: target_id},
                }
            }
        )
        result = acceptance.delete_owned_worker_namespace(
            fake, "synara-lifecycle-test", target_id, 1
        )
        self.assertEqual(fake.deletes, 1)
        self.assertTrue(result["uidPrecondition"])


class FailureReportTest(unittest.TestCase):
    def test_unexpected_exception_is_redacted(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            result_file = Path(directory) / "result.json"
            secret = "must-not-enter-lifecycle-report"
            with mock.patch.object(
                acceptance,
                "run_acceptance",
                side_effect=RuntimeError(secret),
            ):
                status = acceptance.main(
                    [
                        "--context",
                        "orbstack",
                        "--control-plane-namespace",
                        "synara-control",
                        "--worker-namespace",
                        "synara-worker",
                        "--worker-image",
                        "worker:test",
                        "--result-file",
                        str(result_file),
                    ]
                )
            self.assertEqual(status, 1)
            raw = result_file.read_text(encoding="utf-8")
            payload = json.loads(raw)
            self.assertNotIn(secret, raw)
            self.assertEqual(payload["reasonCode"], "unexpected_acceptance_failure")
            self.assertEqual(payload["details"], {"exceptionType": "RuntimeError"})


if __name__ == "__main__":
    unittest.main()
