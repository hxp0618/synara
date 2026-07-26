#!/usr/bin/env python3
"""Focused tests for managed-hook-controller.py (stdlib only)."""

from __future__ import annotations

import datetime as dt
import importlib.util
import json
import os
import pathlib
import signal
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from unittest import mock


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
CONTROLLER_PATH = SCRIPT_DIR / "managed-hook-controller.py"
SPEC = importlib.util.spec_from_file_location("managed_hook_controller", CONTROLLER_PATH)
assert SPEC is not None and SPEC.loader is not None
controller = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = controller
SPEC.loader.exec_module(controller)


HOOK_SOURCE = r'''#!/usr/bin/env python3
import datetime as dt
import json
import os
import pathlib
import subprocess
import sys
import time

mode = sys.argv[1]
artifact_path = pathlib.Path(os.environ["SYNARA_MANAGED_HOOK_ARTIFACT"])
transitions = {"start": "terminal-applied", "verify": "applied", "stop": "terminal-healed"}

if mode == "sleep":
    time.sleep(30)
    raise SystemExit(0)
if mode == "missing":
    raise SystemExit(0)
if mode == "setsid-late":
    marker = sys.argv[2]
    subprocess.Popen(
        [sys.executable, "-c", "import pathlib,sys,time; time.sleep(.35); pathlib.Path(sys.argv[1]).write_text('late')", marker],
        start_new_session=True,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
    )
    raise SystemExit(0)

phase = os.environ["SYNARA_MANAGED_HOOK_PHASE"]
transition = transitions[phase]
observed = dt.datetime.now(dt.timezone.utc)
operation_id = os.environ["SYNARA_MANAGED_HOOK_OPERATION_ID"]
if mode == "pending":
    transition = "pending"
elif mode == "mismatch":
    operation_id = "different-operation-1234"
if mode == "stale":
    observed -= dt.timedelta(hours=1)

payload = {
    "schemaVersion": "synara.managed-hook-transition.v1",
    "operationId": operation_id,
    "phase": phase,
    "transition": transition,
    "context": os.environ["SYNARA_MANAGED_HOOK_CONTEXT"],
    "namespace": os.environ["SYNARA_MANAGED_HOOK_NAMESPACE"],
    "node": os.environ["SYNARA_MANAGED_HOOK_NODE"],
    "nodeUid": os.environ["SYNARA_MANAGED_HOOK_NODE_UID"],
    "pod": os.environ["SYNARA_MANAGED_HOOK_POD"],
    "podUid": os.environ["SYNARA_MANAGED_HOOK_POD_UID"],
    "challenge": os.environ["SYNARA_MANAGED_HOOK_CHALLENGE"],
    "observedAt": observed.isoformat().replace("+00:00", "Z"),
    "checks": [{"name": "provider-check", "status": "passed", "evidenceDigest": "ab" * 32}],
}
artifact_path.write_text(json.dumps(payload), encoding="utf-8")

if mode == "inspect":
    inspection_path = pathlib.Path(sys.argv[2])
    sentinel = "ancestor-authority-secret-sentinel"
    argv_leak = any(sentinel in item for item in sys.argv)
    env_leak = any(sentinel in item for item in os.environ.values())
    inherited = []
    fd_root = pathlib.Path("/proc/self/fd") if pathlib.Path("/proc/self/fd").exists() else pathlib.Path("/dev/fd")
    for entry in fd_root.iterdir():
        try:
            target = os.readlink(entry)
        except OSError:
            continue
        inherited.append(target)
    inspection_path.write_text(json.dumps({"argvLeak": argv_leak, "envLeak": env_leak, "fdTargets": inherited}), encoding="utf-8")
'''


class ManagedHookControllerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        os.chmod(self.root, 0o700)
        self.hook = self.root / "hook.py"
        self.hook.write_text(HOOK_SOURCE, encoding="utf-8")
        self.operation_id = "validation-operation-1234"

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def request(
        self,
        phase: str,
        mode: str,
        *,
        artifact: pathlib.Path | None = None,
        extra_command: list[str] | None = None,
        timeout: int = 3,
    ) -> dict[str, object]:
        artifact = artifact or (self.root / f"{phase}-{time.monotonic_ns()}.json")
        command = [sys.executable, str(self.hook), mode]
        if extra_command:
            command.extend(extra_command)
        return {
            "schemaVersion": controller.REQUEST_SCHEMA,
            "action": "execute",
            "operationId": self.operation_id,
            "phase": phase,
            "backend": "process-group-test",
            "securityBoundary": False,
            "scope": {
                "context": "managed-validation",
                "namespace": "synara-system",
                "node": "validation-node",
                "nodeUid": "node-uid-12345678",
                "pod": "control-plane-0",
                "podUid": "pod-uid-12345678",
                "challenge": "a" * 48,
            },
            "command": command,
            "artifactPath": str(artifact),
            "timeoutSeconds": timeout,
            "stopTimeoutSeconds": 2,
            "freshnessSeconds": 30,
        }

    def run_cli(
        self,
        request: dict[str, object],
        *,
        env: dict[str, str] | None = None,
        timeout: int = 10,
    ) -> tuple[subprocess.CompletedProcess[str], dict[str, object]]:
        completed = subprocess.run(
            [sys.executable, str(CONTROLLER_PATH)],
            input=json.dumps(request),
            text=True,
            capture_output=True,
            env=env,
            timeout=timeout,
            check=False,
        )
        self.assertEqual(completed.stderr, "")
        return completed, json.loads(completed.stdout)

    def test_normal_start_verify_and_stop_transitions(self) -> None:
        for phase, transition in controller.PHASE_TRANSITIONS.items():
            with self.subTest(phase=phase):
                completed, payload = self.run_cli(self.request(phase, "normal"))
                self.assertEqual(completed.returncode, 0)
                self.assertEqual(payload["status"], "passed")
                self.assertEqual(payload["expectedTransition"], transition)
                self.assertFalse(payload["securityBoundary"])
                self.assertTrue(payload["terminationConfirmed"])
                self.assertNotIn("challenge", completed.stdout)
                self.assertNotIn("validation-node", completed.stdout)
                self.assertNotIn(self.operation_id, completed.stdout)
                self.assertEqual(payload["checkCount"], 1)
                self.assertEqual(payload["evidenceDigests"], ["ab" * 32])

    def test_systemd_unsupported_fails_125_before_hook(self) -> None:
        request = controller.Request.parse(
            {
                **self.request("start", "normal"),
                "backend": "systemd-user",
                "securityBoundary": True,
            }
        )
        cancellation = controller.Cancellation()
        backend = controller.SystemdUserBackend(cancellation)
        with mock.patch.object(controller.sys, "platform", "darwin"):
            with self.assertRaises(controller.UnsupportedError):
                backend.preflight()
        self.assertFalse(pathlib.Path(request.artifact_path).exists())

    def test_test_backend_is_restricted_to_validation_context(self) -> None:
        request = self.request("start", "normal")
        request["scope"]["context"] = "production-cluster"  # type: ignore[index]
        completed, payload = self.run_cli(request)
        self.assertEqual(completed.returncode, 125)
        self.assertEqual(payload["reason"], "test-backend-context-forbidden")
        self.assertEqual(payload["status"], "unsupported")

    def test_artifact_mismatch_pending_and_stale_fail_closed(self) -> None:
        expected = {
            "mismatch": "transition-artifact-scope-mismatch",
            "pending": "transition-artifact-scope-mismatch",
            "stale": "transition-artifact-stale",
        }
        for mode, reason in expected.items():
            with self.subTest(mode=mode):
                completed, payload = self.run_cli(self.request("start", mode))
                self.assertEqual(completed.returncode, 1)
                self.assertEqual(payload["status"], "failed")
                self.assertEqual(payload["reason"], reason)
                self.assertTrue(payload["terminationConfirmed"])

    def test_preexisting_artifact_is_rejected_before_hook(self) -> None:
        artifact = self.root / "existing.json"
        artifact.write_text("{}", encoding="utf-8")
        completed, payload = self.run_cli(self.request("start", "normal", artifact=artifact))
        self.assertEqual(completed.returncode, 125)
        self.assertEqual(payload["reason"], "artifact-path-not-fresh")
        self.assertEqual(artifact.read_text(encoding="utf-8"), "{}")

    def test_stop_without_terminal_healed_artifact_fails(self) -> None:
        completed, payload = self.run_cli(self.request("stop", "missing"))
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(payload["reason"], "transition-artifact-unavailable")
        self.assertTrue(payload["terminationConfirmed"])

    def test_setsid_late_apply_never_claims_success(self) -> None:
        marker = self.root / "late-apply"
        completed, payload = self.run_cli(
            self.request("start", "setsid-late", extra_command=[str(marker)])
        )
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(payload["status"], "failed")
        self.assertFalse(payload["securityBoundary"])
        self.assertEqual(payload["reason"], "transition-artifact-unavailable")
        # This explicitly documents why the process-group backend is forbidden
        # outside managed-validation: a new-session child may outlive it.  The
        # production cgroup backend kills all same-unit descendants.
        deadline = time.monotonic() + 2
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertTrue(marker.exists())

    def test_async_pending_fails_then_stop_heals(self) -> None:
        start, start_payload = self.run_cli(self.request("start", "pending"))
        self.assertEqual(start.returncode, 1)
        self.assertEqual(start_payload["status"], "failed")
        stop, stop_payload = self.run_cli(self.request("stop", "normal"))
        self.assertEqual(stop.returncode, 0)
        self.assertEqual(stop_payload["expectedTransition"], "terminal-healed")

    def test_linux_process_state_parser_and_terminal_member_classes(self) -> None:
        summary = controller.MemberSummary.from_states(["R", "S", "Z", "X", "x", None])
        self.assertEqual(
            summary.sanitized(),
            {"executable": 2, "zombie": 1, "deadUpper": 1, "deadLower": 1, "unreadable": 1},
        )
        with mock.patch.object(pathlib.Path, "read_text", return_value="42 (odd ) name) Z 1 2 3"):
            self.assertEqual(controller.process_state(42), "Z")

    def test_cgroup_terminal_members_are_reported_but_not_executable(self) -> None:
        backend = controller.SystemdUserBackend(controller.Cancellation())
        handle = mock.Mock(spec=controller.CgroupHandle)
        with (
            mock.patch.object(controller, "read_cgroup_pids", return_value=[11, 12, 13]),
            mock.patch.object(controller, "read_cgroup_populated", return_value=False),
            mock.patch.object(controller, "process_state", side_effect=["Z", "X", "x"]),
        ):
            summary = backend._wait_cgroup_empty(handle, 1)
        self.assertEqual(summary.executable, 0)
        self.assertEqual(summary.zombie, 1)
        self.assertEqual(summary.dead_upper, 1)
        self.assertEqual(summary.dead_lower, 1)

    def test_executable_cgroup_member_blocks_success(self) -> None:
        request = controller.Request.parse(
            {
                **self.request("start", "normal"),
                "backend": "systemd-user",
                "securityBoundary": True,
            }
        )

        class FakeBackend:
            security_boundary = True

            def __init__(self, _cancellation: object) -> None:
                pass

            def preflight(self) -> None:
                pass

            def execute(self, request: object) -> controller.BackendOutcome:
                return controller.BackendOutcome(
                    0,
                    False,
                    False,
                    True,
                    False,
                    controller.MemberSummary(executable=1),
                )

        with mock.patch.object(controller, "SystemdUserBackend", FakeBackend):
            payload, exit_code = controller.run(request, controller.Cancellation())
        self.assertEqual(exit_code, 1)
        self.assertEqual(payload["reason"], "containment-not-empty")
        self.assertEqual(payload["members"]["executable"], 1)

    def test_direct_child_is_observed_wnowait_then_reaped(self) -> None:
        process = subprocess.Popen(
            [sys.executable, "-c", "pass"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
        )
        deadline = time.monotonic() + 3
        while not controller.child_exited_wnowait(process):
            self.assertLess(time.monotonic(), deadline)
            time.sleep(0.01)
        # WNOWAIT retained the direct child; cleanup code owns the final reap.
        self.assertIsNone(process.returncode)
        self.assertEqual(controller.reap_after_cleanup(process), 0)

    def test_signal_cancellation_stops_child_and_returns_terminal_result(self) -> None:
        request = self.request("start", "sleep", timeout=20)
        process = subprocess.Popen(
            [sys.executable, str(CONTROLLER_PATH)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        assert process.stdin is not None
        process.stdin.write(json.dumps(request))
        process.stdin.close()
        time.sleep(0.2)
        process.send_signal(signal.SIGTERM)
        process.wait(timeout=8)
        assert process.stdout is not None and process.stderr is not None
        payload = json.loads(process.stdout.read())
        stderr = process.stderr.read()
        process.stdout.close()
        process.stderr.close()
        self.assertEqual(stderr, "")
        self.assertEqual(process.returncode, 128 + signal.SIGTERM)
        self.assertEqual(payload["status"], "cancelled")
        self.assertTrue(payload["terminationConfirmed"])

    def test_operation_phase_unit_identity_and_recover_are_deterministic(self) -> None:
        execute = controller.Request.parse(self.request("verify", "normal"))
        recovery_raw = {
            "schemaVersion": controller.REQUEST_SCHEMA,
            "action": "recover",
            "operationId": self.operation_id,
            "phase": "verify",
            "backend": "systemd-user",
            "securityBoundary": True,
        }
        recovery = controller.Request.parse(recovery_raw)
        self.assertEqual(execute.unit_name, recovery.unit_name)
        self.assertTrue(recovery.unit_name.endswith(".service"))

        class FakeBackend:
            security_boundary = True

            def __init__(self, _cancellation: object) -> None:
                pass

            def preflight(self) -> None:
                pass

            def recover(self, request: object) -> controller.BackendOutcome:
                return controller.BackendOutcome(None, False, False, False, True, controller.MemberSummary())

        with mock.patch.object(controller, "SystemdUserBackend", FakeBackend):
            payload, exit_code = controller.run(recovery, controller.Cancellation())
        self.assertEqual(exit_code, 0)
        self.assertEqual(payload["status"], "recovered")
        self.assertFalse(payload["unitFound"])
        self.assertTrue(payload["terminationConfirmed"])

    def test_strict_json_rejects_duplicate_members_and_nonfinite_numbers(self) -> None:
        for payload in (b'{"phase":"start","phase":"stop"}', b'{"timeout":NaN}', b'{"timeout":Infinity}'):
            with self.subTest(payload=payload):
                with self.assertRaises(controller.ControllerError) as raised:
                    controller.strict_json_loads(payload, "invalid-request-json", exit_code=125)
                self.assertEqual(raised.exception.code, "invalid-request-json")

        duplicate_request = (
            '{"schemaVersion":"synara.managed-hook-controller-request.v1",'
            '"schemaVersion":"synara.managed-hook-controller-request.v1"}'
        )
        completed = subprocess.run(
            [sys.executable, str(CONTROLLER_PATH)],
            input=duplicate_request,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(completed.returncode, 125)
        self.assertEqual(json.loads(completed.stdout)["reason"], "invalid-request-json")

        artifact = self.root / "duplicate-artifact.json"
        artifact.write_text('{"phase":"start","phase":"stop"}', encoding="utf-8")
        with self.assertRaises(controller.ControllerError) as artifact_error:
            controller.read_artifact_snapshot(str(artifact))
        self.assertEqual(artifact_error.exception.code, "invalid-transition-artifact-json")

    def test_prelaunch_cancellation_never_calls_execute(self) -> None:
        request = controller.Request.parse(self.request("start", "normal"))
        cancellation = controller.Cancellation()
        cancellation.signal_number = signal.SIGTERM

        class FakeBackend:
            security_boundary = False
            executed = False

            def __init__(self, _cancellation: object) -> None:
                pass

            def preflight(self) -> None:
                pass

            def execute(self, _request: object) -> controller.BackendOutcome:
                FakeBackend.executed = True
                raise AssertionError("cancelled request launched hook")

        with mock.patch.object(controller, "ProcessGroupTestBackend", FakeBackend):
            payload, exit_code = controller.run(request, cancellation)
        self.assertEqual(exit_code, 128 + signal.SIGTERM)
        self.assertEqual(payload["status"], "cancelled")
        self.assertFalse(FakeBackend.executed)

        raw = self.request("start", "normal")
        raw["backend"] = "systemd-user"
        raw["securityBoundary"] = True
        systemd_request = controller.Request.parse(raw)
        systemd_backend = controller.SystemdUserBackend(cancellation)
        systemd_backend.systemd_run = "/usr/bin/systemd-run"
        with mock.patch.object(controller.subprocess, "Popen") as popen:
            outcome = systemd_backend.execute(systemd_request)
        self.assertTrue(outcome.cancelled)
        popen.assert_not_called()

    def test_setsid_nested_cgroup_population_blocks_until_recursive_event_clears(self) -> None:
        backend = controller.SystemdUserBackend(controller.Cancellation())
        handle = mock.Mock(spec=controller.CgroupHandle)
        with (
            mock.patch.object(controller, "read_cgroup_populated", side_effect=[True, False]) as populated,
            mock.patch.object(controller, "read_cgroup_pids", return_value=[]),
            mock.patch.object(controller.time, "sleep"),
        ):
            summary = backend._wait_cgroup_empty(handle, 1)
        self.assertEqual(populated.call_count, 2)
        self.assertEqual(summary.executable, 0)
        self.assertEqual(summary.unreadable, 0)

    def test_opened_cgroup_identity_rejects_path_replacement(self) -> None:
        cgroup = self.root / "example.service"
        cgroup.mkdir()
        (cgroup / "cgroup.events").write_text("populated 1\n", encoding="ascii")
        (cgroup / "cgroup.procs").write_text("", encoding="ascii")
        (cgroup / "cgroup.threads").write_text("", encoding="ascii")
        with mock.patch.object(controller, "cgroup_path", return_value=cgroup):
            handle = controller.open_cgroup("/example.service")
            try:
                self.assertTrue(controller.read_cgroup_populated(handle))
                old = self.root / "old.service"
                cgroup.rename(old)
                cgroup.mkdir()
                (cgroup / "cgroup.events").write_text("populated 0\n", encoding="ascii")
                with self.assertRaises(controller.ControllerError) as raised:
                    controller.read_cgroup_populated(handle)
                self.assertEqual(raised.exception.code, "unit-cgroup-identity-changed")
            finally:
                handle.close()

    def test_systemd_identity_includes_invocation_id(self) -> None:
        backend = controller.SystemdUserBackend(controller.Cancellation())
        completed = subprocess.CompletedProcess(
            args=[],
            returncode=0,
            stdout=(
                b"LoadState=loaded\nControlGroup=/user.slice/example.service\n"
                b"Result=success\nInvocationID=0123456789abcdef\n"
            ),
            stderr=b"",
        )
        with mock.patch.object(backend, "_run_systemctl", return_value=completed) as systemctl:
            state = backend._unit_identity("example.service")
        self.assertEqual(state.invocation_id, "0123456789abcdef")
        self.assertIn("--property=InvocationID", systemctl.call_args.args[0])

    def test_systemd_execute_uses_cgroup_lifetime_properties_and_bound_identity(self) -> None:
        raw = self.request("start", "normal")
        raw["backend"] = "systemd-user"
        raw["securityBoundary"] = True
        request = controller.Request.parse(raw)
        backend = controller.SystemdUserBackend(controller.Cancellation())
        backend.systemd_run = "/usr/bin/systemd-run"
        process = mock.Mock()
        process.pid = 4242
        handle = mock.Mock(spec=controller.CgroupHandle)
        handle.path = "/user.slice/example.service"
        state = controller.UnitState(True, handle.path, "success", "invocation-123")
        with (
            mock.patch.object(controller.subprocess, "Popen", return_value=process) as popen,
            mock.patch.object(controller, "process_identity", return_value="linux-proc-start:77"),
            mock.patch.object(controller, "child_exited_wnowait", side_effect=[False, True]),
            mock.patch.object(controller, "open_cgroup", return_value=handle),
            mock.patch.object(backend, "_unit_identity", return_value=state),
            mock.patch.object(backend, "_cleanup_spawned_unit", return_value=(controller.MemberSummary(), 0)),
            mock.patch.object(controller.time, "sleep"),
        ):
            outcome = backend.execute(request)
        command = popen.call_args.args[0]
        self.assertIn("--collect", command)
        self.assertIn("--property=ExitType=cgroup", command)
        self.assertIn("--property=KillMode=control-group", command)
        self.assertTrue(outcome.termination_confirmed)
        handle.close.assert_called_once()

    def test_postspawn_systemctl_failure_still_runs_exact_unit_cleanup(self) -> None:
        raw = self.request("start", "normal")
        raw["backend"] = "systemd-user"
        raw["securityBoundary"] = True
        request = controller.Request.parse(raw)
        backend = controller.SystemdUserBackend(controller.Cancellation())
        backend.systemd_run = "/usr/bin/systemd-run"
        process = mock.Mock()
        process.pid = 4242
        failure = controller.ControllerError("systemctl-failed", exit_code=125)
        with (
            mock.patch.object(controller.subprocess, "Popen", return_value=process),
            mock.patch.object(controller, "process_identity", return_value="linux-proc-start:77"),
            mock.patch.object(backend, "_unit_identity", side_effect=failure),
            mock.patch.object(backend, "_cleanup_spawned_unit", return_value=(controller.MemberSummary(), 0)) as cleanup,
        ):
            with self.assertRaises(controller.ControllerError) as raised:
                backend.execute(request)
        self.assertEqual(raised.exception.code, "systemctl-failed")
        cleanup.assert_called_once_with(request.unit_name, process, None, request.stop_timeout_seconds)

    def test_cleanup_attempts_cgroup_reap_and_reset_even_when_stop_errors(self) -> None:
        backend = controller.SystemdUserBackend(controller.Cancellation())
        process = mock.Mock()
        handle = mock.Mock(spec=controller.CgroupHandle)
        stop_error = controller.ControllerError("systemctl-failed", exit_code=125)
        with (
            mock.patch.object(backend, "_stop_unit", side_effect=stop_error),
            mock.patch.object(backend, "_wait_cgroup_empty", return_value=controller.MemberSummary()) as empty,
            mock.patch.object(controller, "reap_after_cleanup", return_value=0) as reap,
            mock.patch.object(backend, "_reset_unit") as reset,
            mock.patch.object(backend, "_wait_unit_absent") as absent,
        ):
            with self.assertRaises(controller.ControllerError) as raised:
                backend._cleanup_spawned_unit("example.service", process, handle, 3)
        self.assertEqual(raised.exception.code, "systemd-cleanup-unconfirmed")
        empty.assert_called_once_with(handle, 3)
        reap.assert_called_once_with(process)
        reset.assert_called_once_with("example.service")
        absent.assert_called_once_with("example.service", 3)

    def test_sigkill_recovery_resets_and_collects_stale_same_id_unit(self) -> None:
        recovery = controller.Request.parse(
            {
                "schemaVersion": controller.REQUEST_SCHEMA,
                "action": "recover",
                "operationId": self.operation_id,
                "phase": "verify",
                "backend": "systemd-user",
                "securityBoundary": True,
            }
        )
        backend = controller.SystemdUserBackend(controller.Cancellation())
        stale = controller.UnitState(True, None, "failed", "old-invocation")
        with (
            mock.patch.object(backend, "_unit_identity", return_value=stale),
            mock.patch.object(backend, "_stop_unit") as stop,
            mock.patch.object(backend, "_reset_unit") as reset,
            mock.patch.object(backend, "_wait_unit_absent") as absent,
        ):
            outcome = backend.recover(recovery)
        stop.assert_called_once_with(recovery.unit_name, 5)
        reset.assert_called_once_with(recovery.unit_name)
        absent.assert_called_once_with(recovery.unit_name, 5)
        self.assertTrue(outcome.termination_confirmed)
        execute = controller.Request.parse(
            {**self.request("verify", "normal"), "backend": "systemd-user", "securityBoundary": True}
        )
        self.assertEqual(execute.unit_name, recovery.unit_name)

    def test_no_authority_secret_is_accepted_or_inherited_by_hook(self) -> None:
        marker = self.root / "must-not-run"
        request = self.request("start", "normal")
        request["authoritySecret"] = "secret-sentinel-value"
        request["command"] = [sys.executable, "-c", f"open({str(marker)!r}, 'w').write('bad')"]
        completed, payload = self.run_cli(request)
        self.assertEqual(completed.returncode, 125)
        self.assertEqual(payload["reason"], "invalid-request-fields")
        self.assertFalse(marker.exists())

        inspection = self.root / "inspection.json"
        sentinel = "ancestor-authority-secret-sentinel"
        clean_request = self.request("verify", "inspect", extra_command=[str(inspection)])
        inherited_env = os.environ.copy()
        inherited_env["SYNARA_TEST_UNRELATED_PARENT_VALUE"] = sentinel
        completed, payload = self.run_cli(clean_request, env=inherited_env)
        self.assertEqual(completed.returncode, 0)
        self.assertEqual(payload["status"], "passed")
        observed = json.loads(inspection.read_text(encoding="utf-8"))
        self.assertFalse(observed["argvLeak"])
        self.assertFalse(observed["envLeak"])
        self.assertTrue(all(sentinel not in target for target in observed["fdTargets"]))
        source = CONTROLLER_PATH.read_text(encoding="utf-8")
        self.assertNotIn("import hmac", source)
        self.assertNotIn("authoritySecret", source)


if __name__ == "__main__":
    unittest.main()
