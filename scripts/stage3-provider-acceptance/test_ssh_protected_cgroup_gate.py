from __future__ import annotations

import base64
import contextlib
import dataclasses
import hashlib
import io
import json
import tempfile
import subprocess
import unittest
from pathlib import Path
from typing import Any, Mapping

import ssh_protected_cgroup_gate


class SSHProtectedCgroupGateTest(unittest.TestCase):
    def make_fixture(self) -> tuple[tempfile.TemporaryDirectory[str], ssh_protected_cgroup_gate.SSHProtectedCgroupGateOptions, dict[str, Any]]:
        temporary_directory = tempfile.TemporaryDirectory()
        root = Path(temporary_directory.name)
        identity_file = root / "id_ed25519"
        host_key_file = root / "known_hosts"
        target_capabilities_file = root / "target-capabilities.json"
        worker_manifest_file = root / "worker-manifest.json"
        registration_context_file = root / "registration-context.json"
        identity_file.write_text("PRIVATE KEY PLACEHOLDER\n", encoding="utf-8")
        host_key_file.write_text("known-host placeholder\n", encoding="utf-8")

        public_key = bytes(range(32))
        public_key_base64 = base64.b64encode(public_key).decode("ascii")
        public_key_base64_raw = public_key_base64.rstrip("=")
        probe_sha256 = "ab" * 32
        envelope = {
            "schemaVersion": 1,
            "keyId": "policy-key",
            "signature": base64.b64encode(b"signature").decode("ascii"),
        }
        preflight = {
            "enabled": True,
            "mode": "cgroup-v2",
            "supervisorVersion": "agentd-protected-cgroup-supervisor-v2",
            "probeVersion": 1,
            "probeSha256": probe_sha256,
            "supervisorIdentity": "uid:0 gid:0",
            "providerIdentity": "uid:10001 gid:10002",
            "useCgroupFD": True,
            "setsidDescendantKilled": True,
            "providerUid": 10001,
            "providerGid": 10002,
            "attestationKeyId": "policy-key",
            "attestationPublicKeySha256": hashlib.sha256(public_key).hexdigest(),
            "attestationPublicKeyBase64": public_key_base64,
            "attestation": envelope,
        }
        target_capabilities = {
            "processContainmentPolicy": {
                "trustMode": "signed-v1",
                "keyId": "policy-key",
                "ed25519PublicKey": public_key_base64_raw,
            }
        }
        worker_runtime = {
            "workerBuildVersion": "agentd-1.2.3",
            "workerBuildGitSha": "abcdef0",
            "workerProtocolMinimum": 2,
            "workerProtocolMaximum": 2,
            "runtimeEventMinimum": 2,
            "runtimeEventMaximum": 2,
            "operatingSystem": "linux",
            "architecture": "amd64",
            "imageDigest": "sha256:" + "12" * 32,
            "processContainment": {
                "mode": "cgroup-v2",
                "supervisorVersion": preflight["supervisorVersion"],
                "probeVersion": preflight["probeVersion"],
                "probeSha256": preflight["probeSha256"],
                "supervisorIdentity": preflight["supervisorIdentity"],
                "providerIdentity": preflight["providerIdentity"],
                "attestation": envelope,
            },
        }
        worker_manifest = {
            "capabilities": {
                "workerRuntime": worker_runtime,
            },
            "processContainment": {
                "mode": "cgroup-v2",
                "trustState": "verified",
            },
        }
        registration_context = {
            "executionTargetId": "11111111-1111-1111-1111-111111111111",
            "targetKind": "ssh",
            "instanceUid": "22222222-2222-2222-2222-222222222222",
            "clusterId": "ssh",
            "namespace": "default",
            "podName": "ssh-worker",
        }
        target_capabilities_file.write_text(json.dumps(target_capabilities), encoding="utf-8")
        worker_manifest_file.write_text(json.dumps(worker_manifest), encoding="utf-8")
        registration_context_file.write_text(json.dumps(registration_context), encoding="utf-8")
        options = ssh_protected_cgroup_gate.SSHProtectedCgroupGateOptions(
            ssh_host="ssh.example.internal",
            ssh_port=22,
            ssh_user="synara_admin",
            ssh_identity_file=identity_file,
            ssh_host_key_file=host_key_file,
            service_name="synara-agentd-test.service",
            remote_agentd_binary="/opt/synara/test/synara-agentd",
            remote_env_file="/opt/synara/test/agentd.env",
            target_capabilities_file=target_capabilities_file,
            worker_manifest_file=worker_manifest_file,
            registration_context_file=registration_context_file,
        )
        return temporary_directory, options, {
            "public_key": public_key,
            "preflight": preflight,
            "worker_manifest": worker_manifest,
        }

    def parse_args_argv(self, root: Path, **overrides: str) -> list[str]:
        arguments = {
            "--allow-remote-host": None,
            "--ssh-host": "ssh.example.internal",
            "--ssh-port": "22",
            "--ssh-user": "synara_admin",
            "--ssh-identity-file": str(root / "id_ed25519"),
            "--ssh-host-key-file": str(root / "known_hosts"),
            "--service-name": "synara-agentd-test.service",
            "--remote-agentd-binary": "/opt/synara/test/synara-agentd",
            "--remote-env-file": "/opt/synara/test/agentd.env",
            "--target-capabilities-file": str(root / "target-capabilities.json"),
            "--worker-manifest-file": str(root / "worker-manifest.json"),
            "--registration-context-file": str(root / "registration-context.json"),
        }
        arguments.update(overrides)
        argv: list[str] = []
        for option, value in arguments.items():
            if value is None:
                argv.append(option)
                continue
            argv.extend([option, value])
        return argv

    def live_env_payload(
        self,
        options: ssh_protected_cgroup_gate.SSHProtectedCgroupGateOptions,
        *,
        cgroup_root: str | None = None,
    ) -> str:
        return "\n".join(
            [
                "SYNARA_EXECUTION_TARGET_ID='11111111-1111-1111-1111-111111111111'",
                "SYNARA_EXECUTION_TARGET_KIND='ssh'",
                "SYNARA_AGENTD_CLUSTER_ID='ssh'",
                "SYNARA_AGENTD_NAMESPACE='default'",
                "SYNARA_AGENTD_INSTANCE_ID='ssh-worker'",
                "SYNARA_AGENTD_INSTANCE_UID='22222222-2222-2222-2222-222222222222'",
                "SYNARA_AGENTD_CGROUP_V2_ROOT='" + (
                    cgroup_root or "/sys/fs/cgroup/system.slice/" + options.service_name
                ) + "'",
                "SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID='10001'",
                "SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID='10002'",
                "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID='policy-key'",
                "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE='/etc/synara/keys/process-containment.ed25519'",
                "SYNARA_AGENTD_BUILD_GIT_SHA='abcdef0'",
                "SYNARA_AGENTD_IMAGE_DIGEST='sha256:" + "12" * 32 + "'",
                "SYNARA_AGENTD_VERSION='agentd-1.2.3'",
            ]
        )

    def successful_remote_runner(
        self,
        options: ssh_protected_cgroup_gate.SSHProtectedCgroupGateOptions,
        preflight: Mapping[str, Any],
        *,
        commands: list[str] | None = None,
        active_state: str = "active",
        sub_state: str = "running",
        cgroup_root: str | None = None,
        environment_file: str | None = None,
        process_executable: str | None = None,
        parent_processes: str | None = None,
        final_overrides: Mapping[str, Any] | None = None,
        final_bracket_overrides: Mapping[str, Any] | None = None,
    ) -> Any:
        env_payload = self.live_env_payload(options, cgroup_root=cgroup_root)
        control_group = "/system.slice/" + options.service_name
        after_preflight = False
        after_first_final_core = False
        post_preflight_stat_reads = 0
        final_overrides = dict(final_overrides or {})
        final_bracket_overrides = dict(final_bracket_overrides or {})

        def current(name: str, initial: Any) -> Any:
            if after_preflight and after_first_final_core and name in final_bracket_overrides:
                return final_bracket_overrides[name]
            if after_preflight and name in final_overrides:
                return final_overrides[name]
            return initial

        def remote_runner(_: ssh_protected_cgroup_gate.SSHProtectedCgroupGateOptions, command: str) -> str:
            nonlocal after_preflight, after_first_final_core, post_preflight_stat_reads
            if commands is not None:
                commands.append(command)
            if command.startswith("grep -E "):
                return str(current("env_payload", env_payload))
            if "--property=User" in command:
                return str(current("service_user", "root")) + "\n"
            if "--property=Delegate" in command:
                return str(current("delegate", "yes")) + "\n"
            if "--property=ActiveState" in command:
                return str(current("active_state", active_state)) + "\n"
            if "--property=SubState" in command:
                return str(current("sub_state", sub_state)) + "\n"
            if "--property=MainPID" in command:
                return str(current("main_pid", 4242)) + "\n"
            if "--property=ControlGroup" in command:
                return str(current("control_group", control_group)) + "\n"
            if "--property=EnvironmentFiles" in command:
                return str(current("environment_file", environment_file or str(options.remote_env_file))) + " (ignore_errors=no)\n"
            if command.startswith("cat -- /proc/") and command.endswith("/stat"):
                pid = int(current("main_pid", 4242))
                starttime = int(current("starttime", 987654))
                result = self.proc_stat(pid, starttime)
                if after_preflight:
                    post_preflight_stat_reads += 1
                    # Core 3 has an initial and tail stat read. Switch only
                    # after core 4's initial stat so its own tail bracket must
                    # detect the simulated restart/PID reuse.
                    if post_preflight_stat_reads == 3:
                        after_first_final_core = True
                return result
            if command.startswith("readlink -f /proc/"):
                return str(current("process_executable", process_executable or options.remote_agentd_binary)) + "\n"
            if command.startswith("awk -F:"):
                return str(current("process_control_group", current("control_group", control_group))) + "\n"
            if command.startswith("tr '") and "/environ" in command:
                return str(current("process_env_payload", current("env_payload", env_payload)))
            if command.startswith("stat -fc %T "):
                return str(current("cgroup_filesystem", "cgroup2fs")) + "\n"
            if command.startswith("stat -Lc '%d:%i' "):
                return str(current("cgroup_root_identity", "42:84")) + "\n"
            if command.startswith("cat -- ") and command.endswith("/cgroup.procs"):
                return str(current("parent_processes", parent_processes or f"{current('main_pid', 4242)}\n"))
            if "protected-cgroup-preflight" in command:
                after_preflight = True
                return json.dumps(preflight)
            return ""

        return remote_runner

    @staticmethod
    def proc_stat(pid: int, starttime: int) -> str:
        suffix = ["S", *(["0"] * 18), str(starttime), *(["0"] * 5)]
        return f"{pid} (synara agentd ) worker) {' '.join(suffix)}\n"

    def test_parse_args_requires_allow_remote_host(self) -> None:
        temporary_directory, _, _ = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        root = Path(temporary_directory.name)
        argv = self.parse_args_argv(root)
        argv.remove("--allow-remote-host")
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                ssh_protected_cgroup_gate.parse_args(argv)

    def test_parse_args_rejects_unsafe_host_and_remote_path(self) -> None:
        temporary_directory, _, _ = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        root = Path(temporary_directory.name)
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                ssh_protected_cgroup_gate.parse_args(
                    [
                        "--allow-remote-host",
                        "--ssh-host=-oProxyCommand=sh",
                        "--ssh-port",
                        "22",
                        "--ssh-user",
                        "synara_admin",
                        "--ssh-identity-file",
                        str(root / "id_ed25519"),
                        "--ssh-host-key-file",
                        str(root / "known_hosts"),
                        "--service-name",
                        "synara-agentd-test.service",
                        "--remote-agentd-binary",
                        "/opt/synara/test/synara-agentd",
                        "--remote-env-file",
                        "/opt/synara/test/agentd.env",
                        "--target-capabilities-file",
                        str(root / "target-capabilities.json"),
                        "--worker-manifest-file",
                        str(root / "worker-manifest.json"),
                        "--registration-context-file",
                        str(root / "registration-context.json"),
                    ]
                )
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                ssh_protected_cgroup_gate.parse_args(
                    self.parse_args_argv(root, **{"--remote-env-file": "/opt/synara/../agentd.env"})
                )

    def test_parse_shell_environment_supports_single_quoted_values(self) -> None:
        payload = "\n".join(
            [
                "SYNARA_AGENTD_CGROUP_V2_ROOT='/sys/fs/cgroup/system.slice/synara-agentd.service'",
                "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID='policy-key'",
            ]
        )
        parsed = ssh_protected_cgroup_gate.parse_shell_environment(payload)
        self.assertEqual(
            parsed["SYNARA_AGENTD_CGROUP_V2_ROOT"],
            "/sys/fs/cgroup/system.slice/synara-agentd.service",
        )
        self.assertEqual(parsed["SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID"], "policy-key")

    def test_parse_process_containment_policy_rejects_urlsafe_base64(self) -> None:
        policy = {
            "processContainmentPolicy": {
                "trustMode": "signed-v1",
                "keyId": "policy-key",
                "ed25519PublicKey": "___________________________________________",
            }
        }
        with self.assertRaisesRegex(ValueError, "standard base64"):
            ssh_protected_cgroup_gate.parse_process_containment_policy(policy)

    def test_parse_proc_stat_starttime_handles_spaces_and_parentheses_in_comm(self) -> None:
        payload = self.proc_stat(4242, 987654)
        self.assertEqual(ssh_protected_cgroup_gate.parse_proc_stat_starttime(payload, 4242), 987654)
        with self.assertRaisesRegex(ValueError, "PID does not match"):
            ssh_protected_cgroup_gate.parse_proc_stat_starttime(payload, 5252)

    def test_live_preflight_uses_minimal_quoted_environment_without_sourcing_secrets(self) -> None:
        temporary_directory, options, _ = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        marker = Path(temporary_directory.name) / "command-injection-marker"
        malicious_payload = self.live_env_payload(options).replace(
            "SYNARA_AGENTD_INSTANCE_ID='ssh-worker'",
            f"SYNARA_AGENTD_INSTANCE_ID='worker$(touch {marker})`touch {marker}`'",
        )
        values = ssh_protected_cgroup_gate.parse_shell_environment(malicious_payload)
        values["SYNARA_WORKER_REGISTRATION_TOKEN"] = "REAL_REGISTRATION_SECRET_SENTINEL"
        values["SYNARA_AGENTD_RUNNER_COMMAND_JSON"] = '["secret-provider-command"]'
        safe_options = dataclasses.replace(options, remote_agentd_binary="/usr/bin/printf")
        command = ssh_protected_cgroup_gate.build_live_preflight_command(safe_options, values)
        self.assertTrue(command.startswith("/usr/bin/env -i "))
        self.assertNotIn(options.remote_env_file, command)
        self.assertNotIn("REAL_REGISTRATION_SECRET_SENTINEL", command)
        self.assertNotIn("secret-provider-command", command)
        self.assertIn("ssh-protected-cgroup-preflight-dummy", command)
        self.assertIn("SYNARA_AGENTD_RUNNER_COMMAND_JSON", command)
        subprocess.run(["sh", "-ceu", command], check=True, capture_output=True, text=True)
        self.assertFalse(marker.exists())

    def test_validate_live_preflight_rejects_same_uid(self) -> None:
        env_values = {
            "SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID": "10001",
            "SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID": "10002",
            "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID": "policy-key",
        }
        preflight = {
            "enabled": True,
            "mode": "cgroup-v2",
            "useCgroupFD": True,
            "setsidDescendantKilled": True,
            "probeSha256": "ab" * 32,
            "supervisorIdentity": "uid:10001 gid:0",
            "providerIdentity": "uid:10001 gid:10002",
            "providerUid": 10001,
            "providerGid": 10002,
            "attestationKeyId": "policy-key",
            "attestationPublicKeySha256": "cd" * 32,
            "attestationPublicKeyBase64": base64.b64encode(bytes(range(32))).decode("ascii"),
            "attestation": {"schemaVersion": 1, "keyId": "policy-key", "signature": "c2ln"},
        }
        with self.assertRaisesRegex(ValueError, "supervisor is not root|share a UID"):
            ssh_protected_cgroup_gate.validate_live_preflight("root", "yes", env_values, preflight)

    def test_validate_manifest_trusted_rejects_signed_legacy_supervisor(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        preflight = dict(fixture["preflight"])
        preflight["supervisorVersion"] = "agentd-protected-cgroup-supervisor-v1"
        worker_runtime = dict(fixture["worker_manifest"]["capabilities"]["workerRuntime"])
        containment = dict(worker_runtime["processContainment"])
        containment["supervisorVersion"] = preflight["supervisorVersion"]
        worker_runtime["processContainment"] = containment
        with self.assertRaisesRegex(ValueError, "supervisorVersion is unsupported"):
            ssh_protected_cgroup_gate.validate_manifest_trusted(
                {"workerRuntime": worker_runtime},
                json.loads(options.registration_context_file.read_text(encoding="utf-8")),
                ssh_protected_cgroup_gate.parse_process_containment_policy(
                    json.loads(options.target_capabilities_file.read_text(encoding="utf-8"))
                ),
                preflight,
                verifier=lambda *_: None,
            )


    def test_run_gate_uses_allowlisted_env_and_requires_verified_projection(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        commands: list[str] = []

        remote_runner = self.successful_remote_runner(options, fixture["preflight"], commands=commands)

        verifier_calls: list[tuple[bytes, Mapping[str, Any], Mapping[str, Any]]] = []

        def verifier(public_key: bytes, envelope: Mapping[str, Any], statement: Mapping[str, Any]) -> None:
            verifier_calls.append((public_key, envelope, statement))

        result = ssh_protected_cgroup_gate.run_gate(
            options,
            remote_runner=remote_runner,
            attestation_verifier=verifier,
        )

        self.assertEqual(result["status"], "pass")
        self.assertTrue(result["manifestTrusted"])
        self.assertTrue(commands)
        self.assertIn("grep -E", commands[0])
        self.assertNotIn("cat ", commands[0])
        self.assertFalse(any("SYNARA_WORKER_REGISTRATION_TOKEN_FILE" in command for command in commands))
        self.assertTrue(any("ssh-protected-cgroup-preflight-dummy" in command for command in commands))
        self.assertTrue(any(command.startswith("/usr/bin/env -i ") and "protected-cgroup-preflight" in command for command in commands))
        self.assertFalse(any("set -a" in command or ". " + str(options.remote_env_file) in command for command in commands))
        self.assertEqual(sum("--property=MainPID" in command for command in commands), 8)
        self.assertEqual(sum(command.startswith("cat -- /proc/") and command.endswith("/stat") for command in commands), 8)
        self.assertEqual(len(verifier_calls), 1)
        public_key, envelope, statement = verifier_calls[0]
        self.assertEqual(public_key, fixture["public_key"])
        self.assertEqual(envelope, fixture["preflight"]["attestation"])
        self.assertEqual(statement["workerBuildGitSha"], "abcdef0")
        self.assertEqual(statement["imageDigest"], "sha256:" + "12" * 32)

    def test_run_gate_rejects_unverified_projected_manifest(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        worker_manifest = fixture["worker_manifest"]
        worker_manifest["processContainment"]["trustState"] = "untrusted"
        options.worker_manifest_file.write_text(json.dumps(worker_manifest), encoding="utf-8")

        remote_runner = self.successful_remote_runner(options, fixture["preflight"])

        with self.assertRaisesRegex(ValueError, "trustState is not verified"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_inactive_service(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            active_state="failed",
            sub_state="failed",
        )
        with self.assertRaisesRegex(ValueError, "not active/running"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_additional_service_parent_process(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            parent_processes="4242\n5252\n",
        )
        with self.assertRaisesRegex(ValueError, "service parent must contain only the systemd MainPID"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_main_pid_change_during_preflight(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_overrides={"main_pid": 5252, "parent_processes": "5252\n"},
        )
        with self.assertRaisesRegex(ValueError, "incarnation changed.*mainPid"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_run_gate_rejects_same_pid_reuse_during_preflight(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_overrides={"starttime": 987655},
        )
        with self.assertRaisesRegex(ValueError, "incarnation changed.*processStartTime"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_final_snapshot_bracket_rejects_same_pid_reuse_after_first_sample(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_bracket_overrides={"starttime": 987656},
        )
        with self.assertRaisesRegex(ValueError, "within core snapshot.*processStartTime"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_final_snapshot_bracket_rejects_restart_after_first_sample(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_bracket_overrides={"main_pid": 6262, "parent_processes": "6262\n"},
        )
        with self.assertRaisesRegex(ValueError, "service parent must contain only|within core snapshot.*mainPid"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_final_snapshot_bracket_rejects_user_delegate_and_root_inode_changes(self) -> None:
        for name, overrides, expected in (
            ("user", {"service_user": "nobody"}, "serviceUser"),
            ("delegate", {"delegate": "no"}, "delegate"),
            ("root-inode", {"cgroup_root_identity": "42:85"}, "cgroupRootIdentity"),
        ):
            with self.subTest(name=name):
                temporary_directory, options, fixture = self.make_fixture()
                self.addCleanup(temporary_directory.cleanup)
                remote_runner = self.successful_remote_runner(
                    options,
                    fixture["preflight"],
                    final_bracket_overrides=overrides,
                )
                with self.assertRaisesRegex(ValueError, "within (?:core snapshot|one live-host snapshot).*" + expected):
                    ssh_protected_cgroup_gate.run_gate(
                        options,
                        remote_runner=remote_runner,
                        attestation_verifier=lambda *_: None,
                    )

    def test_run_gate_rejects_bound_environment_change_during_preflight(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        changed_env = self.live_env_payload(options).replace("SYNARA_AGENTD_BUILD_GIT_SHA='abcdef0'", "SYNARA_AGENTD_BUILD_GIT_SHA='abcdef1'")
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_overrides={"env_payload": changed_env, "process_env_payload": changed_env},
        )
        with self.assertRaisesRegex(ValueError, "incarnation changed.*env"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_run_gate_rejects_executable_change_during_preflight(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_overrides={"process_executable": "/opt/synara/test/restarted-agentd"},
        )
        with self.assertRaisesRegex(ValueError, "does not execute the expected agentd binary"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_run_gate_rejects_control_group_change_during_preflight(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        changed_control_group = "/system.slice/synara-agentd-restarted.service"
        changed_root = "/sys/fs/cgroup" + changed_control_group
        changed_env = self.live_env_payload(options, cgroup_root=changed_root)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            final_overrides={
                "control_group": changed_control_group,
                "process_control_group": changed_control_group,
                "env_payload": changed_env,
                "process_env_payload": changed_env,
            },
        )
        with self.assertRaisesRegex(ValueError, "incarnation changed.*controlGroup"):
            ssh_protected_cgroup_gate.run_gate(options, remote_runner=remote_runner, attestation_verifier=lambda *_: None)

    def test_run_gate_rejects_cgroup_root_outside_service_control_group(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            cgroup_root="/sys/fs/cgroup/system.slice/unrelated.service",
        )
        with self.assertRaisesRegex(ValueError, "not the systemd service delegated ControlGroup"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_unbound_environment_file(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            environment_file="/opt/synara/test/unrelated.env",
        )
        with self.assertRaisesRegex(ValueError, "not bound to the expected EnvironmentFile"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_unbound_process_executable(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        remote_runner = self.successful_remote_runner(
            options,
            fixture["preflight"],
            process_executable="/opt/synara/test/unrelated-agentd",
        )
        with self.assertRaisesRegex(ValueError, "does not execute the expected agentd binary"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )

    def test_run_gate_rejects_registration_context_not_bound_to_live_env(self) -> None:
        temporary_directory, options, fixture = self.make_fixture()
        self.addCleanup(temporary_directory.cleanup)
        registration_context = json.loads(options.registration_context_file.read_text(encoding="utf-8"))
        registration_context["podName"] = "forged-worker"
        options.registration_context_file.write_text(json.dumps(registration_context), encoding="utf-8")
        remote_runner = self.successful_remote_runner(options, fixture["preflight"])
        with self.assertRaisesRegex(ValueError, "podName does not match"):
            ssh_protected_cgroup_gate.run_gate(
                options,
                remote_runner=remote_runner,
                attestation_verifier=lambda *_: None,
            )


if __name__ == "__main__":
    unittest.main()
