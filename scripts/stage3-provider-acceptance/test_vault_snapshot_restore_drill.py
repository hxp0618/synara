from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import vault_snapshot_restore_drill as drill


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
OPERATIONS_POLICY_PATH = REPO_ROOT / "deploy/kubernetes/security/vault/operations-policy.json"
SNAPSHOT_OPERATOR_POLICY_PATH = (
    REPO_ROOT / "deploy/kubernetes/security/vault/synara-vault-snapshot-operator.hcl"
)

SOURCE_STATUS = {
    "initialized": True,
    "sealed": False,
    "storage_type": "raft",
    "version": "2.0.3",
    "ha_enabled": True,
    "cluster_id": "source-cluster-id",
}
PREINIT_STATUS = {
    "initialized": False,
    "sealed": True,
    "storage_type": "raft",
    "version": "2.0.3",
    "ha_enabled": True,
    "cluster_id": "restore-cluster-id",
}
RESTORED_SEALED_STATUS = {
    "initialized": True,
    "sealed": True,
    "storage_type": "raft",
    "version": "2.0.3",
    "ha_enabled": True,
    "cluster_id": "restore-cluster-id",
}
LOCAL_INIT_UNSEALED_STATUS = {
    "initialized": True,
    "sealed": False,
    "progress": 0,
    "t": 3,
    "n": 5,
    "storage_type": "raft",
    "version": "2.0.3",
    "ha_enabled": True,
    "cluster_id": "restore-cluster-id",
}
RESTORED_UNSEALED_STATUS = {
    "initialized": True,
    "sealed": False,
    "progress": 0,
    "t": 3,
    "n": 5,
    "storage_type": "raft",
    "version": "2.0.3",
    "ha_enabled": True,
    "cluster_id": "restore-cluster-id",
}
SOURCE_RAFT = {
    "data": {
        "config": {
            "servers": [
                {"node_id": "source-0", "address": "https://source-0:8201", "leader": True, "voter": True},
                {"node_id": "source-1", "address": "https://source-1:8201", "leader": False, "voter": True},
                {"node_id": "source-2", "address": "https://source-2:8201", "leader": False, "voter": True},
            ]
        }
    }
}
RESTORED_RAFT = {
    "data": {
        "config": {
            "servers": [
                {"node_id": "restore-0", "address": "http://127.0.0.1:8201", "leader": True, "voter": True}
            ]
        }
    }
}
TRANSIT_KEY = {
    "data": {
        "name": "synara-worker-release",
        "type": "ecdsa-p256",
        "latest_version": 1,
        "min_available_version": 1,
    }
}
SIGNER_ROLE = {
    "data": {
        "token_policies": ["synara-worker-release-signer"],
        "token_type": "batch",
        "token_ttl": 7200,
        "token_max_ttl": 14400,
        "token_num_uses": 0,
        "secret_id_ttl": 600,
        "secret_id_num_uses": 1,
        "token_no_default_policy": True,
    }
}
AUDITOR_ROLE = {
    "data": {
        "token_policies": ["synara-vault-production-auditor"],
        "token_type": "batch",
        "token_ttl": 1800,
        "token_max_ttl": 3600,
        "token_num_uses": 0,
        "secret_id_ttl": 600,
        "secret_id_num_uses": 1,
        "token_no_default_policy": True,
    }
}
SNAPSHOT_OPERATOR_ROLE = {
    "data": {
        "bind_secret_id": True,
        "token_policies": ["synara-vault-snapshot-operator"],
        "token_type": "batch",
        "token_ttl": 1800,
        "token_max_ttl": 3600,
        "token_num_uses": 0,
        "secret_id_ttl": 600,
        "secret_id_num_uses": 1,
        "token_no_default_policy": True,
    }
}
AUDIT_DEVICES = {
    "file/": {
        "type": "file",
        "options": {"file_path": "/vault/audit/audit-primary.log"},
    },
    "file-secondary/": {
        "type": "file",
        "options": {"file_path": "/vault/audit/audit-secondary.log"},
    },
}
AUDIT_FILE_METADATA = {
    "primary": {"mode": "600", "uid": 100, "gid": 1000, "sizeBytes": 512},
    "secondary": {"mode": "600", "uid": 100, "gid": 1000, "sizeBytes": 512},
}
SOURCE_LOGIN = {
    "auth": {
        "client_token": "source-auth-token-value",
        "accessor": "source-accessor",
        "display_name": "approle",
        "lease_duration": 1800,
        "renewable": False,
        "token_policies": ["synara-vault-snapshot-operator"],
        "token_type": "batch",
    }
}
TOKEN_LOOKUP = {
    "data": {
        "display_name": "approle",
        "path": "auth/approle/login",
        "policies": ["synara-vault-snapshot-operator"],
        "type": "batch",
    }
}
RESTORE_INIT = {
    "root_token": "restore-root-token-value",
    "unseal_keys_b64": [
        "restore-init-unseal-one",
        "restore-init-unseal-two",
        "restore-init-unseal-three",
        "restore-init-unseal-four",
        "restore-init-unseal-five",
    ],
}


def completed(
    *,
    args: list[str] | tuple[str, ...] | None = None,
    returncode: int = 0,
    stdout: str = "",
    stderr: str = "",
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(args=args or [], returncode=returncode, stdout=stdout, stderr=stderr)


class FakeVaultRestoreRunner:
    def __init__(
        self,
        *,
        drift_restored_signer: bool = False,
        context_cancel_on_final_unseal: bool = False,
    ) -> None:
        self.drift_restored_signer = drift_restored_signer
        self.context_cancel_on_final_unseal = context_cancel_on_final_unseal
        self.status_calls = 0
        self.unseal_calls = 0
        self.unseal_commands: list[str] = []
        self.local_unseal_fallback_pending = False

    def __call__(
        self,
        executable: str,
        arguments: list[str] | tuple[str, ...],
        *,
        cwd: pathlib.Path,
        environment: dict[str, str] | None = None,
        input_text: str | None = None,
        timeout: float,
        code: str,
        message: str,
    ) -> subprocess.CompletedProcess[str]:
        environment = environment or {}
        joined = " ".join(arguments)

        for secret in (
            "source-auth-token-value",
            "source-secret-id-value",
            "restore-root-token-value",
            "restore-share-one",
            "restore-share-two",
            "restore-share-three",
        ):
            if secret in joined:
                raise AssertionError(f"secret entered command arguments: {secret}")

        if executable == "/bin/sh" and "auth/approle/login" in joined:
            return completed(args=arguments, stdout=json.dumps(SOURCE_LOGIN))

        if executable == "vault":
            if list(arguments) == ["token", "lookup", "-format=json"]:
                return completed(args=arguments, stdout=json.dumps(TOKEN_LOOKUP))
            if list(arguments) == ["status", "-format=json"]:
                return completed(args=arguments, stdout=json.dumps(SOURCE_STATUS))
            if list(arguments) == ["read", "-format=json", "sys/storage/raft/configuration"]:
                return completed(args=arguments, stdout=json.dumps(SOURCE_RAFT))
            if list(arguments) == ["read", "-format=json", "transit/keys/synara-worker-release"]:
                return completed(args=arguments, stdout=json.dumps(TRANSIT_KEY))
            if list(arguments) == ["audit", "list", "-format=json"]:
                return completed(args=arguments, stdout=json.dumps(AUDIT_DEVICES))
            if list(arguments) == ["read", "-format=json", "auth/approle/role/synara-worker-release-signer"]:
                return completed(args=arguments, stdout=json.dumps(SIGNER_ROLE))
            if list(arguments) == ["read", "-format=json", "auth/approle/role/synara-vault-production-auditor"]:
                return completed(args=arguments, stdout=json.dumps(AUDITOR_ROLE))
            if list(arguments) == ["read", "-format=json", "auth/approle/role/synara-vault-snapshot-operator"]:
                return completed(args=arguments, stdout=json.dumps(SNAPSHOT_OPERATOR_ROLE))
            if list(arguments[:4]) == ["operator", "raft", "snapshot", "save"]:
                pathlib.Path(arguments[4]).write_bytes(b"stage3-snapshot")
                return completed(args=arguments)

        if executable == "docker":
            if arguments and arguments[0] == "run":
                self.assert_no_duplicate_config(arguments)
                if drill.DEFAULT_RESTORE_AUDIT_TMPFS not in arguments:
                    raise AssertionError("the isolated restore container did not mount the hardened audit tmpfs")
                return completed(args=arguments, stdout="container-id\n")
            if arguments and arguments[0] == "logs":
                return completed(
                    args=arguments,
                    stdout=(
                        "post-unseal setup complete\n"
                        "shutting down prior to restoring snapshot\n"
                        "applying snapshot\n"
                        "raft snapshot restore failed postUnseal\n"
                    ),
                )
            if arguments and arguments[0] == "cp":
                return completed(args=arguments)
            if arguments and arguments[0] == "rm":
                return completed(args=arguments, stdout="removed\n")
            if arguments and arguments[0] == "exec":
                if "status -format=json" in joined:
                    if self.local_unseal_fallback_pending:
                        self.local_unseal_fallback_pending = False
                        return completed(args=arguments, stdout=json.dumps(LOCAL_INIT_UNSEALED_STATUS))
                    self.status_calls += 1
                    if self.status_calls == 1:
                        return completed(args=arguments, returncode=2, stdout=json.dumps(PREINIT_STATUS))
                    if self.status_calls == 2:
                        return completed(
                            args=arguments,
                            returncode=2,
                            stdout=json.dumps(RESTORED_SEALED_STATUS),
                        )
                    return completed(args=arguments, stdout=json.dumps(RESTORED_UNSEALED_STATUS))
                if "vault operator init -key-shares=5 -key-threshold=3 -format=json" in joined:
                    return completed(args=arguments, stdout=json.dumps(RESTORE_INIT))
                if "/v1/sys/unseal" in joined:
                    self.unseal_commands.append(joined)
                    self.unseal_calls += 1
                    if self.unseal_calls in (1, 4):
                        return completed(
                            args=arguments,
                            stdout=json.dumps({"sealed": True, "progress": 1, "t": 3, "n": 5}),
                        )
                    if self.unseal_calls in (2, 5):
                        return completed(
                            args=arguments,
                            stdout=json.dumps({"sealed": True, "progress": 2, "t": 3, "n": 5}),
                        )
                    if self.unseal_calls in (3, 6) and self.context_cancel_on_final_unseal:
                        if self.unseal_calls == 3:
                            self.local_unseal_fallback_pending = True
                        return completed(
                            args=arguments,
                            returncode=22,
                            stdout=json.dumps({"errors": ["context canceled"]}),
                        )
                    return completed(
                        args=arguments,
                        stdout=json.dumps({"sealed": False, "progress": 0, "t": 3, "n": 5}),
                    )
                if "operator raft snapshot restore -force" in joined:
                    return completed(args=arguments)
                if "token lookup -format=json" in joined:
                    return completed(args=arguments, stdout=json.dumps(TOKEN_LOOKUP))
                if "read -format=json sys/storage/raft/configuration" in joined:
                    return completed(args=arguments, stdout=json.dumps(RESTORED_RAFT))
                if "read -format=json transit/keys/synara-worker-release" in joined:
                    return completed(args=arguments, stdout=json.dumps(TRANSIT_KEY))
                if "audit list -format=json" in joined:
                    return completed(args=arguments, stdout=json.dumps(AUDIT_DEVICES))
                if "primary=/vault/audit/audit-primary.log" in joined:
                    return completed(args=arguments, stdout=json.dumps(AUDIT_FILE_METADATA))
                if "read -format=json auth/approle/role/synara-worker-release-signer" in joined:
                    payload = SIGNER_ROLE
                    if self.drift_restored_signer:
                        payload = {
                            "data": {
                                **SIGNER_ROLE["data"],
                                "token_ttl": 7199,
                            }
                        }
                    return completed(args=arguments, stdout=json.dumps(payload))
                if "read -format=json auth/approle/role/synara-vault-production-auditor" in joined:
                    return completed(args=arguments, stdout=json.dumps(AUDITOR_ROLE))
                if "read -format=json auth/approle/role/synara-vault-snapshot-operator" in joined:
                    return completed(args=arguments, stdout=json.dumps(SNAPSHOT_OPERATOR_ROLE))

        raise AssertionError(
            f"unexpected command executable={executable!r} arguments={arguments!r} env_keys={sorted(environment)}"
        )

    @staticmethod
    def assert_no_duplicate_config(arguments: list[str] | tuple[str, ...]) -> None:
        if any(str(argument).startswith("-config=") for argument in arguments):
            raise AssertionError("the Vault image entrypoint already loads /vault/config")


class VaultSnapshotRestoreDrillTests(unittest.TestCase):
    def test_checked_in_policy_and_snapshot_operator_policy_match_stage3_baseline(self) -> None:
        policy = drill.load_operations_policy(OPERATIONS_POLICY_PATH)
        text, sha256 = drill.load_snapshot_operator_policy(SNAPSHOT_OPERATOR_POLICY_PATH)

        self.assertEqual(policy.kms_reference, "hashivault://synara-worker-release")
        self.assertEqual(policy.snapshot_operator_role_name, "synara-vault-snapshot-operator")
        self.assertEqual(policy.custody_total_shares, 5)
        self.assertEqual(policy.custody_threshold, 3)
        self.assertEqual(policy.audit_devices, drill.EXPECTED_AUDIT_DEVICES)
        self.assertEqual(policy.restore_audit_tmpfs, drill.DEFAULT_RESTORE_AUDIT_TMPFS)
        self.assertTrue(policy.audit_siem_required)
        self.assertEqual(policy.snapshot_operator_role_policy.token_type, "batch")
        self.assertEqual(policy.snapshot_operator_role_policy.secret_id_num_uses, 1)
        self.assertRegex(sha256, r"^[0-9a-f]{64}$")
        self.assertIn('path "sys/storage/raft/snapshot"', text)
        self.assertNotIn("capabilities = [\"update\"]", text)

    def test_dirty_repository_fails_before_vault_or_secret_actions(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            output_dir = pathlib.Path(temp_dir) / "report"
            options = drill.GateOptions(
                repo_root=REPO_ROOT,
                output_dir=output_dir,
                operations_policy_path=OPERATIONS_POLICY_PATH,
                snapshot_operator_policy_path=SNAPSHOT_OPERATOR_POLICY_PATH,
                vault_bin="vault",
                docker_bin="docker",
                timeout_seconds=30.0,
                vault_client_timeout="10m",
            )

            def dirty_repository(_repo_root: pathlib.Path) -> dict[str, object]:
                raise drill.ReleaseGateError(
                    "release.worktree_dirty",
                    "The release gate requires a clean Git worktree.",
                )

            with mock.patch.object(drill, "_run_command") as run_command:
                result = drill.run_vault_snapshot_restore_drill(
                    options,
                    repository_state_loader=dirty_repository,
                )

            self.assertEqual(result, 1)
            run_command.assert_not_called()
            report = json.loads((output_dir / drill.JSON_REPORT_NAME).read_text(encoding="utf-8"))
            self.assertEqual(report["errors"][0]["code"], "release.worktree_dirty")
            self.assertTrue(report["cleanup"]["exactOwnerCleanup"])

    def test_successful_restore_drill_writes_secret_safe_reports(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            output_dir = pathlib.Path(temp_dir) / "report"
            options = drill.GateOptions(
                repo_root=REPO_ROOT,
                output_dir=output_dir,
                operations_policy_path=OPERATIONS_POLICY_PATH,
                snapshot_operator_policy_path=SNAPSHOT_OPERATOR_POLICY_PATH,
                vault_bin="vault",
                docker_bin="docker",
                timeout_seconds=30.0,
                vault_client_timeout="10m",
            )
            runner = FakeVaultRestoreRunner()
            env = {
                "VAULT_ADDR": "https://source-vault.example:8200",
                "VAULT_CACERT": __file__,
                "VAULT_SNAPSHOT_OPERATOR_ROLE_ID": "source-role-id-value",
                "VAULT_SNAPSHOT_OPERATOR_SECRET_ID": "source-secret-id-value",
                "VAULT_SNAPSHOT_RESTORE_KEY_1": "restore-share-one",
                "VAULT_SNAPSHOT_RESTORE_KEY_2": "restore-share-two",
                "VAULT_SNAPSHOT_RESTORE_KEY_3": "restore-share-three",
            }
            with (
                mock.patch.dict(os.environ, env, clear=False),
                mock.patch.object(drill, "_run_command", side_effect=runner),
                mock.patch.object(drill.time, "sleep", return_value=None),
                mock.patch.object(
                    drill.uuid,
                    "uuid4",
                    return_value=types.SimpleNamespace(hex="0123456789abcdef0123456789abcdef"),
                ),
            ):
                result = drill.run_vault_snapshot_restore_drill(
                    options,
                    repository_state_loader=lambda _root: {
                        "gitSha": "a" * 40,
                        "worktreeDirty": False,
                    },
                )

            self.assertEqual(result, 0)
            json_path = output_dir / drill.JSON_REPORT_NAME
            markdown_path = output_dir / drill.MARKDOWN_REPORT_NAME
            report = json.loads(json_path.read_text(encoding="utf-8"))
            raw_report = json_path.read_text(encoding="utf-8") + markdown_path.read_text(encoding="utf-8")

            self.assertEqual(report["status"], "pass")
            self.assertEqual(report["source"]["gitSha"], "a" * 40)
            self.assertFalse(report["source"]["worktreeDirty"])
            self.assertTrue(report["validation"]["transitKeyHashMatch"])
            self.assertTrue(report["validation"]["auditDeviceHashMatch"])
            self.assertTrue(report["validation"]["signerRoleHashMatch"])
            self.assertTrue(report["cleanup"]["exactOwnerCleanup"])
            self.assertEqual(
                report["source"]["credentials"]["snapshotOperatorRoleIdEnvironment"],
                "VAULT_SNAPSHOT_OPERATOR_ROLE_ID",
            )
            self.assertEqual(
                report["restore"]["dockerNetworkMode"],
                "none",
            )
            self.assertTrue(report["restore"]["snapshotApplication"]["completed"])
            self.assertEqual(report["restore"]["raft"]["leaderCount"], 1)
            self.assertEqual(report["restore"]["raft"]["voterCount"], 1)
            self.assertEqual(len(runner.unseal_commands), 6)
            self.assertTrue(all("nc -w 15" in command for command in runner.unseal_commands))
            self.assertTrue(all("sleep 1" not in command for command in runner.unseal_commands))
            self.assertNotIn("source-secret-id-value", raw_report)
            self.assertNotIn("source-role-id-value", raw_report)
            self.assertNotIn("restore-share-one", raw_report)

    def test_context_cancel_after_final_unseal_uses_status_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            output_dir = pathlib.Path(temp_dir) / "report"
            options = drill.GateOptions(
                repo_root=REPO_ROOT,
                output_dir=output_dir,
                operations_policy_path=OPERATIONS_POLICY_PATH,
                snapshot_operator_policy_path=SNAPSHOT_OPERATOR_POLICY_PATH,
                vault_bin="vault",
                docker_bin="docker",
                timeout_seconds=30.0,
                vault_client_timeout="10m",
            )
            runner = FakeVaultRestoreRunner(context_cancel_on_final_unseal=True)
            env = {
                "VAULT_ADDR": "https://source-vault.example:8200",
                "VAULT_CACERT": __file__,
                "VAULT_SNAPSHOT_OPERATOR_ROLE_ID": "source-role-id-value",
                "VAULT_SNAPSHOT_OPERATOR_SECRET_ID": "source-secret-id-value",
                "VAULT_SNAPSHOT_RESTORE_KEY_1": "restore-share-one",
                "VAULT_SNAPSHOT_RESTORE_KEY_2": "restore-share-two",
                "VAULT_SNAPSHOT_RESTORE_KEY_3": "restore-share-three",
            }
            with (
                mock.patch.dict(os.environ, env, clear=False),
                mock.patch.object(drill, "_run_command", side_effect=runner),
                mock.patch.object(drill.time, "sleep", return_value=None),
            ):
                result = drill.run_vault_snapshot_restore_drill(
                    options,
                    repository_state_loader=lambda _root: {
                        "gitSha": "a" * 40,
                        "worktreeDirty": False,
                    },
                )

            self.assertEqual(result, 0)
            report = json.loads((output_dir / drill.JSON_REPORT_NAME).read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "pass")
            self.assertEqual(report["restore"]["unsealAttempts"][-1]["sealed"], False)

    def test_restore_drift_fails_but_cleanup_stays_exact(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            output_dir = pathlib.Path(temp_dir) / "report"
            options = drill.GateOptions(
                repo_root=REPO_ROOT,
                output_dir=output_dir,
                operations_policy_path=OPERATIONS_POLICY_PATH,
                snapshot_operator_policy_path=SNAPSHOT_OPERATOR_POLICY_PATH,
                vault_bin="vault",
                docker_bin="docker",
                timeout_seconds=30.0,
                vault_client_timeout="10m",
            )
            runner = FakeVaultRestoreRunner(drift_restored_signer=True)
            env = {
                "VAULT_ADDR": "https://source-vault.example:8200",
                "VAULT_CACERT": __file__,
                "VAULT_SNAPSHOT_OPERATOR_ROLE_ID": "source-role-id-value",
                "VAULT_SNAPSHOT_OPERATOR_SECRET_ID": "source-secret-id-value",
                "VAULT_SNAPSHOT_RESTORE_KEY_1": "restore-share-one",
                "VAULT_SNAPSHOT_RESTORE_KEY_2": "restore-share-two",
                "VAULT_SNAPSHOT_RESTORE_KEY_3": "restore-share-three",
            }
            with (
                mock.patch.dict(os.environ, env, clear=False),
                mock.patch.object(drill, "_run_command", side_effect=runner),
                mock.patch.object(drill.time, "sleep", return_value=None),
                mock.patch.object(
                    drill.uuid,
                    "uuid4",
                    return_value=types.SimpleNamespace(hex="fedcba9876543210fedcba9876543210"),
                ),
            ):
                result = drill.run_vault_snapshot_restore_drill(
                    options,
                    repository_state_loader=lambda _root: {
                        "gitSha": "a" * 40,
                        "worktreeDirty": False,
                    },
                )

            self.assertEqual(result, 1)
            report = json.loads((output_dir / drill.JSON_REPORT_NAME).read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "fail")
            self.assertTrue(report["cleanup"]["exactOwnerCleanup"])
            self.assertFalse(report["validation"]["signerRoleHashMatch"])
            self.assertTrue(
                any(
                    error.get("code") == "release.vault_snapshot_restore_validation_failed"
                    for error in report["errors"]
                )
            )


if __name__ == "__main__":
    unittest.main()
