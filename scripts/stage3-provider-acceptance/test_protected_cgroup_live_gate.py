from __future__ import annotations

import hashlib
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock

import protected_cgroup_live_gate


class ProtectedCgroupLiveGateTest(unittest.TestCase):
    def make_options(self, root: pathlib.Path) -> protected_cgroup_live_gate.Options:
        return protected_cgroup_live_gate.Options(
            orbctl="orbctl",
            vm_name="synara-cgroup-v2-live-20260726cleanup",
            output=root / "report.json",
            timeout=120,
        )

    def debian_machine(self) -> dict[str, object]:
        return {
            "id": "01DEBIANVMID",
            "name": "debian",
            "state": "running",
            "image": {"distro": "debian", "version": "trixie", "arch": "arm64"},
        }

    def owned_machine(self, machine_id: str = "01OWNEDVMID") -> dict[str, object]:
        return {
            "id": machine_id,
            "name": "synara-cgroup-v2-live-20260726cleanup",
            "state": "running",
            "image": {"distro": "ubuntu", "version": "noble", "arch": "arm64"},
        }

    def test_vm_name_requires_exact_owned_prefix_and_safe_token(self) -> None:
        protected_cgroup_live_gate.validate_vm_name("synara-cgroup-v2-live-20260726abc123")
        for value in (
            "debian",
            "synara-cgroup-v2-live-short",
            "synara-cgroup-v2-live-UPPERCASE12",
            "synara-cgroup-v2-live-20260726/other",
        ):
            with self.subTest(value=value), self.assertRaises(ValueError):
                protected_cgroup_live_gate.validate_vm_name(value)

    def test_parse_test_evidence_requires_exact_passing_scenario_set(self) -> None:
        scenarios = {
            "runtimeDiagnosticOverlapAndFence": {"status": "pass"},
            "parentExtraPidZeroMutation": {"status": "pass"},
            "legacyV1ZeroMutation": {"status": "pass"},
            "unknownChildRepairRecovery": {"status": "pass"},
            "sigkillHolderOrphanRecovery": {"status": "pass"},
        }
        payload = {
            "schemaVersion": "synara.protected-cgroup-v2-live-test.v1",
            "scenarios": scenarios,
        }
        output = "=== RUN live\n    file.go:1: " + protected_cgroup_live_gate.TEST_EVIDENCE_PREFIX + json.dumps(payload) + "\n--- PASS\n"
        self.assertEqual(protected_cgroup_live_gate.parse_test_evidence(output), payload)

        scenarios["legacyV1ZeroMutation"] = {"status": "fail"}
        with self.assertRaises(protected_cgroup_live_gate.GateFailure):
            protected_cgroup_live_gate.parse_test_evidence(
                protected_cgroup_live_gate.TEST_EVIDENCE_PREFIX
                + json.dumps({"schemaVersion": payload["schemaVersion"], "scenarios": scenarios})
            )

    def test_stable_machine_identity_ignores_drift_prone_ip_but_binds_vm(self) -> None:
        machine = {
            "id": "01ABC",
            "name": "debian",
            "state": "running",
            "image": {"distro": "debian", "version": "trixie", "arch": "arm64"},
            "ipv4": "192.0.2.10",
        }
        self.assertEqual(
            protected_cgroup_live_gate.stable_machine_identity(machine),
            {
                "id": "01ABC",
                "name": "debian",
                "state": "running",
                "distro": "debian",
                "version": "trixie",
                "architecture": "arm64",
            },
        )

    def test_inventory_preflight_refuses_existing_owned_name(self) -> None:
        inventory = [
            {
                "id": "debian-id",
                "name": "debian",
                "state": "running",
                "image": {"distro": "debian", "version": "trixie", "arch": "arm64"},
            },
            {
                "id": "owned-id",
                "name": "synara-cgroup-v2-live-20260726existing",
                "state": "stopped",
                "image": {"distro": "ubuntu", "version": "noble", "arch": "arm64"},
            },
        ]
        with self.assertRaises(protected_cgroup_live_gate.GateFailure):
            protected_cgroup_live_gate.validate_inventory_preflight(
                inventory, "synara-cgroup-v2-live-20260726existing"
            )

    def test_bounded_output_is_byte_bounded(self) -> None:
        value = "界" * 100
        bounded = protected_cgroup_live_gate.bounded(value, 17)
        self.assertIn("[output truncated]", bounded)
        self.assertLess(len(bounded.encode("utf-8")), 64)

    def test_initial_failure_boundary_never_claims_containment(self) -> None:
        boundary = protected_cgroup_live_gate.initial_evidence_boundary()
        self.assertFalse(boundary["containmentScenariosProved"])
        self.assertEqual(boundary["hostPrerequisitesObserved"], [])
        self.assertNotIn("proved", boundary)

    def test_cleanup_deletes_only_captured_id_and_preserves_debian(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            debian = self.debian_machine()
            owned = self.owned_machine()
            report = {
                "vm": {"name": options.vm_name, "ownedByThisRun": True, "deleted": None},
                "preservedVm": {
                    "before": protected_cgroup_live_gate.stable_machine_identity(debian),
                    "after": None,
                    "unchanged": False,
                },
            }
            commands: list[list[str]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                return subprocess.CompletedProcess(command, 0, "")

            with (
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[[debian, owned], [debian]],
                ),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
            ):
                errors = protected_cgroup_live_gate.cleanup_proven_owned_vm(
                    options,
                    report,
                    "01OWNEDVMID",
                    protected_cgroup_live_gate.stable_machine_identity(debian),
                )
            self.assertEqual(errors, [])
            self.assertEqual(commands, [["orbctl", "delete", "--force", "01OWNEDVMID"]])
            self.assertTrue(report["vm"]["deleted"])
            self.assertTrue(report["preservedVm"]["unchanged"])

    def test_id_delete_panic_requires_manual_cleanup_and_never_uses_name(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            debian = self.debian_machine()
            owned = self.owned_machine()
            report = {
                "vm": {"name": options.vm_name, "ownedByThisRun": True, "deleted": None},
                "preservedVm": {
                    "before": protected_cgroup_live_gate.stable_machine_identity(debian),
                    "after": None,
                    "unchanged": False,
                },
            }
            commands: list[list[str]] = []
            panic_output = "panic: runtime error: invalid memory address\n"

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                if command == ["orbctl", "delete", "--force", "01OWNEDVMID"]:
                    return subprocess.CompletedProcess(command, 2, panic_output)
                return subprocess.CompletedProcess(command, 0, "")

            with (
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        [debian, owned],
                        [debian, owned],
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
            ):
                errors = protected_cgroup_live_gate.cleanup_proven_owned_vm(
                    options,
                    report,
                    "01OWNEDVMID",
                    protected_cgroup_live_gate.stable_machine_identity(debian),
                )
            self.assertEqual(commands, [["orbctl", "delete", "--force", "01OWNEDVMID"]])
            self.assertTrue(
                any("manual operator cleanup" in error for error in errors)
            )
            self.assertEqual(report["vm"]["deleteMode"], "opaque-id-failed-no-name-fallback")
            self.assertTrue(report["vm"]["manualCleanupRequired"]["required"])
            self.assertEqual(
                report["vm"]["idDeleteFailure"]["outputSha256"],
                hashlib.sha256(panic_output.encode()).hexdigest(),
            )
            self.assertFalse(report["vm"]["deleted"])

    def test_id_delete_failure_with_replacement_never_uses_name_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            debian = self.debian_machine()
            owned = self.owned_machine()
            replacement = self.owned_machine("01REPLACEVMID")
            report = {
                "vm": {"name": options.vm_name, "ownedByThisRun": True, "deleted": None},
                "preservedVm": {
                    "before": protected_cgroup_live_gate.stable_machine_identity(debian),
                    "after": None,
                    "unchanged": False,
                },
            }
            commands: list[list[str]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                return subprocess.CompletedProcess(command, 2, "opaque-ID panic")

            with (
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        [debian, owned],
                        [debian, replacement],
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
            ):
                errors = protected_cgroup_live_gate.cleanup_proven_owned_vm(
                    options,
                    report,
                    "01OWNEDVMID",
                    protected_cgroup_live_gate.stable_machine_identity(debian),
                )
            self.assertEqual(commands, [["orbctl", "delete", "--force", "01OWNEDVMID"]])
            self.assertTrue(any("manual operator cleanup" in error for error in errors))
            self.assertTrue(report["vm"]["manualCleanupRequired"]["required"])
            self.assertTrue(report["vm"]["deleted"])

    def test_concurrent_lock_loser_never_runs_or_deletes_winner(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            reports: list[dict[str, object]] = []
            with protected_cgroup_live_gate.acquire_vm_name_lock(options.vm_name):
                with (
                    mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                    mock.patch.object(protected_cgroup_live_gate, "run_command") as command,
                    mock.patch.object(
                        protected_cgroup_live_gate,
                        "write_report",
                        side_effect=lambda _path, report: reports.append(dict(report)),
                    ),
                    self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "name lock"),
                ):
                    protected_cgroup_live_gate.run_gate(options)
            command.assert_not_called()
            self.assertEqual(len(reports), 1)
            self.assertFalse(reports[0]["vm"]["ownedByThisRun"])

    def test_lock_rejects_symlink_and_permissive_paths(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            private_target = root / "private-target"
            private_target.mkdir(mode=0o700)
            symlink_directory = root / "symlink-locks"
            symlink_directory.symlink_to(private_target, target_is_directory=True)
            with self.assertRaises(protected_cgroup_live_gate.GateFailure):
                with protected_cgroup_live_gate.acquire_vm_name_lock(
                    "synara-cgroup-v2-live-20260726symlink",
                    symlink_directory,
                ):
                    pass

            permissive_directory = root / "permissive-locks"
            permissive_directory.mkdir(mode=0o755)
            with self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "private owner-controlled directory"):
                with protected_cgroup_live_gate.acquire_vm_name_lock(
                    "synara-cgroup-v2-live-20260726permissive",
                    permissive_directory,
                ):
                    pass

            private_directory = root / "private-locks"
            private_directory.mkdir(mode=0o700)
            lock_name = "synara-cgroup-v2-live-20260726filemode.lock"
            lock_path = private_directory / lock_name
            lock_path.write_text("", encoding="utf-8")
            lock_path.chmod(0o644)
            with self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "private owner-controlled regular file"):
                with protected_cgroup_live_gate.acquire_vm_name_lock(
                    "synara-cgroup-v2-live-20260726filemode",
                    private_directory,
                ):
                    pass

            lock_path.chmod(0o600)
            lock_path.unlink()
            lock_path.symlink_to(root / "elsewhere")
            with self.assertRaises(protected_cgroup_live_gate.GateFailure):
                with protected_cgroup_live_gate.acquire_vm_name_lock(
                    "synara-cgroup-v2-live-20260726filemode",
                    private_directory,
                ):
                    pass

    def test_inventory_replacement_during_marker_proof_is_never_owned_or_deleted(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            created = self.owned_machine("01CREATEDVMID")
            replacement = self.owned_machine("01REPLACEVMID")
            commands: list[list[str]] = []
            reports: list[dict[str, object]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                stdout = "abc123\n" if command[:3] == ["git", "rev-parse", "HEAD"] else ""
                return subprocess.CompletedProcess(command, 0, stdout)

            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(protected_cgroup_live_gate, "source_manifest", return_value=("a" * 64, [])),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        baseline,
                        [*baseline, created],
                        [*baseline, replacement],
                        [*baseline, replacement],
                        [*baseline, replacement],
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "verify_creation_marker"),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "reconcile_late_creation",
                    return_value=protected_cgroup_live_gate.LateCreationReconciliation(
                        identity=None,
                        last_candidate=protected_cgroup_live_gate.stable_machine_identity(replacement),
                        attempts=5,
                        last_error="replacement marker mismatch",
                    ),
                ),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=lambda _path, report: reports.append(dict(report)),
                ),
                self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "identity changed"),
            ):
                protected_cgroup_live_gate.run_gate(options)
            self.assertFalse(any(command[:3] == ["orbctl", "delete", "--force"] for command in commands))
            self.assertFalse(reports[-1]["vm"]["ownedByThisRun"])
            self.assertEqual(reports[-1]["vm"]["unprovenCandidate"]["id"], "01REPLACEVMID")
            self.assertTrue(any("unproven same-name candidate" in error for error in reports[-1]["cleanupErrors"]))

    def test_timed_out_create_with_proved_marker_deletes_exact_id_but_fails_gate(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            owned = self.owned_machine()
            commands: list[list[str]] = []
            reports: list[dict[str, object]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                if len(command) > 1 and command[0:2] == ["orbctl", "create"]:
                    raise subprocess.TimeoutExpired(command, 180)
                stdout = "abc123\n" if command[:3] == ["git", "rev-parse", "HEAD"] else ""
                return subprocess.CompletedProcess(command, 0, stdout)

            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(protected_cgroup_live_gate, "source_manifest", return_value=("a" * 64, [])),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[baseline, [*baseline, owned], [*baseline, owned], [*baseline, owned], baseline],
                ),
                mock.patch.object(protected_cgroup_live_gate, "verify_creation_marker"),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=lambda _path, report: reports.append(dict(report)),
                ),
                self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "marker proved ownership for cleanup"),
            ):
                protected_cgroup_live_gate.run_gate(options)
            self.assertIn(["orbctl", "delete", "--force", "01OWNEDVMID"], commands)
            self.assertTrue(reports[-1]["vm"]["ownedByThisRun"])
            self.assertEqual(reports[-1]["vm"]["createOutcome"], "ambiguous-marker-proved")
            self.assertTrue(reports[-1]["vm"]["deleted"])

    def test_late_reconciliation_handles_delayed_vm_and_marker_without_real_sleep(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            owned = self.owned_machine()

            class FakeClock:
                def __init__(self) -> None:
                    self.now = 0.0

                def clock(self) -> float:
                    return self.now

                def sleep(self, duration: float) -> None:
                    self.now += duration

            delayed_clock = FakeClock()
            inventory_calls = 0

            def delayed_inventory() -> list[dict[str, object]]:
                nonlocal inventory_calls
                inventory_calls += 1
                return baseline if inventory_calls < 5 else [*baseline, owned]

            delayed_vm = protected_cgroup_live_gate.reconcile_late_creation(
                options,
                "a" * 64,
                timeout=10,
                clock=delayed_clock.clock,
                sleeper=delayed_clock.sleep,
                inventory_loader=delayed_inventory,
                marker_verifier=lambda: None,
            )
            self.assertEqual(delayed_vm.attempts, 5)
            self.assertEqual(delayed_vm.identity["id"], "01OWNEDVMID")

            marker_clock = FakeClock()
            marker_attempts = 0

            def delayed_marker() -> None:
                nonlocal marker_attempts
                marker_attempts += 1
                if marker_attempts < 3:
                    raise protected_cgroup_live_gate.GateFailure("marker not ready")

            delayed_marker_result = protected_cgroup_live_gate.reconcile_late_creation(
                options,
                "a" * 64,
                timeout=10,
                clock=marker_clock.clock,
                sleeper=marker_clock.sleep,
                inventory_loader=lambda: [*baseline, owned],
                marker_verifier=delayed_marker,
            )
            self.assertEqual(delayed_marker_result.attempts, 3)
            self.assertEqual(delayed_marker_result.identity["id"], "01OWNEDVMID")

    def test_late_reconciliation_deadline_records_absent_and_unproven_candidates(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            owned = self.owned_machine()

            class FakeClock:
                def __init__(self) -> None:
                    self.now = 0.0

                def clock(self) -> float:
                    return self.now

                def sleep(self, duration: float) -> None:
                    self.now += duration

            absent_clock = FakeClock()
            absent = protected_cgroup_live_gate.reconcile_late_creation(
                options,
                "a" * 64,
                timeout=0.25,
                clock=absent_clock.clock,
                sleeper=absent_clock.sleep,
                inventory_loader=lambda: baseline,
                marker_verifier=lambda: None,
            )
            self.assertIsNone(absent.identity)
            self.assertIsNone(absent.last_candidate)
            self.assertEqual(absent.last_error, "same-name candidate not present")

            unproved_clock = FakeClock()
            unproved = protected_cgroup_live_gate.reconcile_late_creation(
                options,
                "a" * 64,
                timeout=0.25,
                clock=unproved_clock.clock,
                sleeper=unproved_clock.sleep,
                inventory_loader=lambda: [*baseline, owned],
                marker_verifier=lambda: (_ for _ in ()).throw(
                    protected_cgroup_live_gate.GateFailure("marker mismatch")
                ),
            )
            self.assertIsNone(unproved.identity)
            self.assertEqual(unproved.last_candidate["id"], "01OWNEDVMID")
            self.assertEqual(unproved.last_error, "marker mismatch")

    def test_post_create_inventory_failure_never_deletes_unproven_same_name(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            winner = self.owned_machine("01WINNERVMID")
            commands: list[list[str]] = []
            reports: list[dict[str, object]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                stdout = "abc123\n" if command[:3] == ["git", "rev-parse", "HEAD"] else ""
                return subprocess.CompletedProcess(command, 0, stdout)

            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(protected_cgroup_live_gate, "source_manifest", return_value=("a" * 64, [])),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        baseline,
                        protected_cgroup_live_gate.GateFailure("post-create inventory failed"),
                        [*baseline, winner],
                        [*baseline, winner],
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "verify_creation_marker"),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "reconcile_late_creation",
                    return_value=protected_cgroup_live_gate.LateCreationReconciliation(
                        identity=None,
                        last_candidate=protected_cgroup_live_gate.stable_machine_identity(winner),
                        attempts=5,
                        last_error="marker unavailable",
                    ),
                ),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=lambda _path, report: reports.append(dict(report)),
                ),
                self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "post-create inventory failed"),
            ):
                protected_cgroup_live_gate.run_gate(options)
            self.assertFalse(any(command[:3] == ["orbctl", "delete", "--force"] for command in commands))
            self.assertFalse(reports[-1]["vm"]["ownedByThisRun"])
            self.assertIsNone(reports[-1]["vm"]["deleted"])

    def test_final_inventory_failure_still_deletes_exact_id_and_writes_report(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            owned = self.owned_machine()
            commands: list[list[str]] = []
            reports: list[dict[str, object]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                commands.append(command)
                stdout = "abc123\n" if command[:3] == ["git", "rev-parse", "HEAD"] else ""
                return subprocess.CompletedProcess(command, 0, stdout)

            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(protected_cgroup_live_gate, "source_manifest", return_value=("a" * 64, [])),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        baseline,
                        [*baseline, owned],
                        [*baseline, owned],
                        [*baseline, owned],
                        protected_cgroup_live_gate.GateFailure("final inventory failed"),
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "verify_creation_marker"),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "collect_host_facts",
                    side_effect=protected_cgroup_live_gate.GateFailure("stop after ownership"),
                ),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=lambda _path, report: reports.append(dict(report)),
                ),
                self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "stop after ownership"),
            ):
                protected_cgroup_live_gate.run_gate(options)
            self.assertIn(["orbctl", "delete", "--force", "01OWNEDVMID"], commands)
            self.assertEqual(len(reports), 1)
            self.assertTrue(reports[0]["vm"]["ownedByThisRun"])
            self.assertTrue(reports[0]["vm"]["deleteAttempted"])
            self.assertTrue(any("final inventory unavailable" in error for error in reports[0]["cleanupErrors"]))

    def test_delete_failure_is_reported_and_does_not_hide_primary_error(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            baseline = [self.debian_machine()]
            owned = self.owned_machine()
            reports: list[dict[str, object]] = []

            def fake_command(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                stdout = "abc123\n" if command[:3] == ["git", "rev-parse", "HEAD"] else ""
                if command == ["orbctl", "delete", "--force", "01OWNEDVMID"]:
                    return subprocess.CompletedProcess(command, 1, "delete failed")
                return subprocess.CompletedProcess(command, 0, stdout)

            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(protected_cgroup_live_gate, "source_manifest", return_value=("a" * 64, [])),
                mock.patch.object(protected_cgroup_live_gate, "run_command", side_effect=fake_command),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=[
                        baseline,
                        [*baseline, owned],
                        [*baseline, owned],
                        [*baseline, owned],
                        [*baseline, owned],
                    ],
                ),
                mock.patch.object(protected_cgroup_live_gate, "verify_creation_marker"),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "collect_host_facts",
                    side_effect=protected_cgroup_live_gate.GateFailure("primary failure"),
                ),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=lambda _path, report: reports.append(dict(report)),
                ),
                self.assertRaisesRegex(protected_cgroup_live_gate.GateFailure, "primary failure"),
            ):
                protected_cgroup_live_gate.run_gate(options)
            self.assertEqual(reports[0]["status"], "fail")
            self.assertFalse(reports[0]["vm"]["deleted"])
            self.assertTrue(
                any("manual operator cleanup" in error for error in reports[0]["cleanupErrors"])
            )
            self.assertTrue(reports[0]["vm"]["manualCleanupRequired"]["required"])

    def test_primary_and_permanent_report_failure_use_preopened_fallback_and_aggregate(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = pathlib.Path(temporary_directory)
            options = self.make_options(root)
            with (
                mock.patch.object(protected_cgroup_live_gate, "repository_root", return_value=root),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "load_inventory",
                    side_effect=protected_cgroup_live_gate.GateFailure("inventory offline"),
                ),
                mock.patch.object(
                    protected_cgroup_live_gate,
                    "write_report",
                    side_effect=OSError("report disk blocked"),
                ) as writer,
                self.assertRaises(protected_cgroup_live_gate.GateFailure) as raised,
            ):
                protected_cgroup_live_gate.run_gate(options)
            message = str(raised.exception)
            self.assertIn("primary=inventory offline", message)
            self.assertIn("cleanup=final inventory unavailable", message)
            self.assertIn("report=report disk blocked; report disk blocked", message)
            self.assertIn("fallback=", message)
            self.assertEqual(writer.call_count, 2)
            fallback_files = list(root.glob("report.json.fallback-*.json"))
            self.assertEqual(len(fallback_files), 1)
            fallback = json.loads(fallback_files[0].read_text(encoding="utf-8"))
            self.assertEqual(fallback["error"], "inventory offline")
            self.assertEqual(fallback["reportWriteErrors"], ["report disk blocked", "report disk blocked"])


if __name__ == "__main__":
    unittest.main()
