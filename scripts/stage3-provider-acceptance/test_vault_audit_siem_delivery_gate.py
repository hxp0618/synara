from __future__ import annotations

import contextlib
import hashlib
import json
import os
import pathlib
import re
import sys
import tempfile
import threading
import time
import types
import unittest
from collections.abc import Callable
from typing import Any
from unittest import mock

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

acceptance_stub = types.ModuleType("acceptance_runner")


class EnvironmentValueError(RuntimeError):
    pass


class SecretRedactor:
    def __init__(self) -> None:
        self._values: list[tuple[str, str]] = []

    def add(self, value: str | None, replacement: str = "[REDACTED]") -> None:
        if value:
            self._values.append((value, replacement))
            self._values.sort(key=lambda item: len(item[0]), reverse=True)

    def text(self, value: str) -> str:
        for secret, replacement in self._values:
            value = value.replace(secret, replacement)
        return value

    def value(self, value: Any) -> Any:
        if isinstance(value, str):
            return self.text(value)
        if isinstance(value, list):
            return [self.value(item) for item in value]
        if isinstance(value, tuple):
            return [self.value(item) for item in value]
        if isinstance(value, dict):
            return {str(key): self.value(item) for key, item in value.items()}
        return value

    def secret_values(self) -> tuple[str, ...]:
        return tuple(secret for secret, _ in self._values if secret)


def read_environment_value(
    environment_name: str,
    description: str,
    *,
    maximum_length: int,
    forbidden_characters: str,
) -> str:
    value = os.environ.get(environment_name)
    if value is None or not value.strip():
        raise EnvironmentValueError(
            "missing",
            f"The configured {description} environment variable was missing or empty.",
        )
    if len(value) > maximum_length or any(character in value for character in forbidden_characters):
        raise EnvironmentValueError(
            "invalid",
            f"The configured {description} environment value was invalid.",
        )
    return value


def scan_output_secrets(output_dir: pathlib.Path, redactor: SecretRedactor) -> dict[str, Any]:
    findings: list[dict[str, Any]] = []
    patterns = (
        ("private-key", re.compile(r"BEGIN [A-Z0-9 ]*PRIVATE KEY")),
        ("certificate", re.compile(r"BEGIN CERTIFICATE")),
    )
    scanned_files = 0
    scanned_bytes = 0
    for path in sorted(output_dir.rglob("*")):
        if not path.is_file() or path.suffix.lower() not in {".json", ".md", ".txt", ".yaml", ".yml"}:
            continue
        scanned_files += 1
        text = path.read_text(encoding="utf-8")
        scanned_bytes += len(text.encode("utf-8"))
        for index, secret in enumerate(redactor.secret_values(), start=1):
            if secret and secret in text:
                findings.append({"file": str(path.relative_to(output_dir)), "kind": f"known-secret-{index}"})
        for kind, pattern in patterns:
            if pattern.search(text):
                findings.append({"file": str(path.relative_to(output_dir)), "kind": kind})
    return {
        "status": "pass" if not findings else "fail",
        "scannedFiles": scanned_files,
        "scannedBytes": scanned_bytes,
        "findings": findings,
    }


acceptance_stub.EnvironmentValueError = EnvironmentValueError
acceptance_stub.SecretRedactor = SecretRedactor
acceptance_stub.read_environment_value = read_environment_value
acceptance_stub.scan_output_secrets = scan_output_secrets
sys.modules["acceptance_runner"] = acceptance_stub

import release_gate_common as common
import vault_audit_siem_delivery_gate as gate
from test_vault_audit_acceptance_sink import (
    https_json_request,
    request_event,
    running_sink,
)


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
POLICY_PATH = REPO_ROOT / "deploy" / "kubernetes" / "security" / "vault" / "operations-policy.json"


def write_executable(path: pathlib.Path, content: str) -> pathlib.Path:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)
    return path


def gate_options(
    output_dir: pathlib.Path,
    *,
    vault_command: tuple[str, ...],
    kubectl_bin: str = "kubectl",
) -> gate.GateOptions:
    return gate.GateOptions(
        repo_root=REPO_ROOT,
        output_dir=output_dir,
        operations_policy_path=POLICY_PATH,
        vault_command=vault_command,
        vault_auditor_token_env="VAULT_OPERATOR_TOKEN",
        kubectl_bin=kubectl_bin,
        kube_context=None,
        vault_namespace="synara-kms",
        vault_statefulset="synara-vault",
        shipper_container="vault-audit-shipper",
        timeout_seconds=5.0,
        poll_interval_seconds=0.1,
    )


def write_vault_wrapper(path: pathlib.Path, request_id: str) -> pathlib.Path:
    return write_executable(
        path,
        "\n".join(
            [
                "#!/usr/bin/env python3",
                "import json",
                "import os",
                "import sys",
                "assert sys.argv[-3:] == ['read', '-format=json', 'sys/audit']",
                "assert os.environ['VAULT_TOKEN'] == 'vault-operator-super-secret'",
                f"print(json.dumps({{'request_id': '{request_id}', 'data': {{'file/': {{}}, 'file-secondary/': {{}}}}}}))",
            ]
        )
        + "\n",
    )


def write_kubectl_stub(path: pathlib.Path, request_id: str) -> pathlib.Path:
    return write_executable(
        path,
        "\n".join(
            [
                "#!/usr/bin/env python3",
                "import json",
                "import sys",
                "argv = sys.argv[1:]",
                "if 'get' in argv and 'statefulset' in argv:",
                "    print(json.dumps({",
                "        'spec': {",
                "            'template': {",
                "                'spec': {",
                "                    'containers': [",
                "                        {",
                "                            'name': 'vault-audit-shipper',",
                "                            'image': 'timberio/vector:0.45.0-debian@sha256:987a15ebfb2eac3a4d5efb26252d140f799553feffb753dc215bdf738a7d4174'",
                "                        }",
                "                    ]",
                "                }",
                "            }",
                "        },",
                "        'status': {'readyReplicas': 3, 'replicas': 3}",
                "    }))",
                "elif 'logs' in argv:",
                "    print('Vector v0.45.0 started')",
                "else:",
                "    raise SystemExit('unexpected kubectl argv: ' + ' '.join(argv))",
            ]
        )
        + "\n",
    )


def start_async_receipt_post(base_url: str, tls_paths: dict[str, pathlib.Path], request_id: str) -> threading.Thread:
    def worker() -> None:
        time.sleep(0.2)
        https_json_request(
            base_url,
            "/v1/audit/events",
            method="POST",
            ca_cert=tls_paths["ca_cert"],
            client_cert=tls_paths["client_cert"],
            client_key=tls_paths["client_key"],
            body=json.dumps(request_event(request_id)),
        )

    thread = threading.Thread(target=worker, daemon=True)
    thread.start()
    return thread


def stable_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")


class SharedObjectLockStore:
    def __init__(self) -> None:
        self._next_version = 1
        self._objects: dict[str, list[dict[str, Any]]] = {}

    def put(
        self,
        object_key: str,
        content: bytes,
        *,
        metadata_entry_sha256: str | None = None,
    ) -> gate.object_lock.ObjectVersionEvidence:
        version_id = f"fixture-version-{self._next_version}"
        self._next_version += 1
        record = {
            "versionId": version_id,
            "content": content,
            "contentSha256": hashlib.sha256(content).hexdigest(),
            "etag": f"fixture-etag-{version_id}",
            "retainUntil": "2099-01-01T00:00:00Z",
            "retentionMode": "COMPLIANCE",
            "metadataEntrySha256": metadata_entry_sha256,
        }
        self._objects.setdefault(object_key, []).append(record)
        return self.evidence(object_key, record)

    def get(self, object_key: str, version_id: str | None = None) -> dict[str, Any]:
        versions = self._objects.get(object_key)
        if not versions:
            raise KeyError(object_key)
        if version_id is None:
            return versions[-1]
        for record in versions:
            if record["versionId"] == version_id:
                return record
        raise KeyError(f"{object_key}@{version_id}")

    def evidence(
        self,
        object_key: str,
        record: dict[str, Any],
    ) -> gate.object_lock.ObjectVersionEvidence:
        return gate.object_lock.ObjectVersionEvidence(
            object_key=object_key,
            version_id=str(record["versionId"]),
            etag=str(record["etag"]),
            content_sha256=str(record["contentSha256"]),
            retain_until=str(record["retainUntil"]),
            retention_mode=str(record["retentionMode"]),
            metadata_entry_sha256=record["metadataEntrySha256"],
        )

    def only_archive_object_key(self) -> str:
        if len(self._objects) != 1:
            raise AssertionError(f"expected exactly one archived object, found {sorted(self._objects)}")
        return next(iter(self._objects))

    def rewrite_latest_entries(
        self,
        mutate: Callable[[list[dict[str, Any]]], None],
    ) -> None:
        object_key = self.only_archive_object_key()
        record = self._objects[object_key][-1]
        entries = [
            json.loads(line)
            for raw_line in record["content"].decode("utf-8").splitlines()
            if (line := raw_line.strip())
        ]
        mutate(entries)
        content = b"".join(stable_json_bytes(entry) + b"\n" for entry in entries)
        record["content"] = content
        record["contentSha256"] = hashlib.sha256(content).hexdigest()


class FixtureObjectLockClient:
    def __init__(
        self,
        store: SharedObjectLockStore,
        *,
        retention_days: int,
        delete_denial_kind: str | None = "object_lock",
        shorten_denial_kind: str | None = "object_lock",
    ) -> None:
        self.store = store
        self.retention_days = retention_days
        self.delete_denial_kind = delete_denial_kind
        self.shorten_denial_kind = shorten_denial_kind
        self.options = types.SimpleNamespace(
            bucket="synara-vault-audit",
            prefix="entries",
        )

    def verify_bucket_contract(self) -> Any:
        return gate.object_lock.BucketContract(
            versioning_status="Enabled",
            default_retention_mode="COMPLIANCE",
            default_retention_days=self.retention_days,
        )

    def qualify_object_key(self, basename: str) -> str:
        return f"{self.options.prefix}/{basename}"

    def upload_bytes(self, object_key: str, content: bytes) -> str:
        return self.store.put(object_key, content).content_sha256

    def put_bytes(self, object_key: str, content: bytes) -> gate.object_lock.ObjectVersionEvidence:
        return self.store.put(object_key, content)

    def verify_existing_object(
        self,
        object_key: str,
        *,
        expected_content_sha256: str,
        version_id: str | None = None,
        required_entry_sha256: str | None = None,
    ) -> gate.object_lock.ObjectVersionEvidence:
        record = self.store.get(object_key, version_id)
        if record["contentSha256"] != expected_content_sha256:
            raise gate.object_lock.S3ObjectLockError(
                "s3_object_lock.content_hash_drift",
                "fixture content drift",
            )
        metadata_entry_sha256 = record["metadataEntrySha256"]
        if required_entry_sha256 is not None:
            if metadata_entry_sha256 is None:
                raise gate.object_lock.S3ObjectLockError(
                    "s3_object_lock.entry_sha256_missing",
                    "fixture metadata entry sha missing",
                )
            if metadata_entry_sha256 != required_entry_sha256:
                raise gate.object_lock.S3ObjectLockError(
                    "s3_object_lock.entry_sha256_mismatch",
                    "fixture metadata entry sha drifted",
                )
        return self.store.evidence(object_key, record)

    def cat_version(self, object_key: str, version_id: str) -> bytes:
        return bytes(self.store.get(object_key, version_id)["content"])

    def probe_delete_version(self, _object_key: str, _version_id: str) -> Any:
        return self._negative_probe("deleteVersion", self.delete_denial_kind)

    def probe_shorten_retention(self, _object_key: str, _version_id: str) -> Any:
        return self._negative_probe("shortenRetention", self.shorten_denial_kind)

    def _negative_probe(self, operation: str, denial_kind: str | None) -> gate.object_lock.NegativeProbeResult:
        if denial_kind == "object_lock":
            return gate.object_lock.NegativeProbeResult(
                operation=operation,
                blocked=True,
                return_code=1,
                statuses=("failure", "error"),
                error_codes=("ObjectLocked", "WORM", "AccessDenied"),
                denial_kind="object_lock",
            )
        if denial_kind == "iam":
            return gate.object_lock.NegativeProbeResult(
                operation=operation,
                blocked=True,
                return_code=1,
                statuses=("error",),
                error_codes=("AccessDenied",),
                denial_kind="iam",
            )
        return gate.object_lock.NegativeProbeResult(
            operation=operation,
            blocked=False,
            return_code=0,
            statuses=("success",),
            error_codes=(),
            denial_kind=None,
        )


def build_gate_object_lock_clients(
    store: SharedObjectLockStore,
    *,
    retention_days: int,
    writer_delete_denial_kind: str | None = "iam",
    verifier_delete_denial_kind: str | None = "object_lock",
    verifier_shorten_denial_kind: str | None = "object_lock",
) -> gate.ObjectLockClients:
    return gate.ObjectLockClients(
        writer=FixtureObjectLockClient(
            store,
            retention_days=retention_days,
            delete_denial_kind=writer_delete_denial_kind,
            shorten_denial_kind="iam",
        ),
        verifier=FixtureObjectLockClient(
            store,
            retention_days=retention_days,
            delete_denial_kind=verifier_delete_denial_kind,
            shorten_denial_kind=verifier_shorten_denial_kind,
        ),
    )


def object_lock_environment(
    root: pathlib.Path,
    *,
    writer_host: str = "https://fixture-user:fixture-secret@object-lock.example.test",
    verifier_host: str = "https://fixture-verifier:fixture-verifier-secret@object-lock.example.test",
) -> dict[str, str]:
    config_dir = root / "mc-config"
    config_dir.mkdir()
    return {
        "VAULT_AUDIT_WORM_MC_ALIAS": "FIXTURE",
        "VAULT_AUDIT_WORM_MC_CONFIG_DIR": str(config_dir),
        "VAULT_AUDIT_WORM_MC_HOST": writer_host,
        "VAULT_AUDIT_WORM_MC_VERIFIER_HOST": verifier_host,
        "VAULT_AUDIT_WORM_MC_RESOLVE": "object-lock.example.test:443=127.0.0.1",
    }


def gate_environment(
    root: pathlib.Path,
    fixture: dict[str, Any],
    *,
    object_lock_values: dict[str, str] | None = None,
) -> dict[str, str]:
    return {
        "PATH": os.environ.get("PATH", ""),
        "HOME": os.environ.get("HOME", str(root)),
        "LANG": "C.UTF-8",
        "VAULT_ADDR": "https://vault.example.test",
        "VAULT_CACERT": fixture["tls"]["ca_cert"].read_text(encoding="utf-8"),
        "VAULT_OPERATOR_TOKEN": "vault-operator-super-secret",
        "VAULT_AUDIT_SIEM_ENDPOINT": fixture["base_url"],
        "VAULT_AUDIT_SIEM_RESOLVE": f"{fixture['base_url'].removeprefix('https://')}=127.0.0.1",
        "VAULT_AUDIT_SIEM_CLIENT_CERT": fixture["tls"]["client_cert"].read_text(encoding="utf-8"),
        "VAULT_AUDIT_SIEM_CLIENT_KEY": fixture["tls"]["client_key"].read_text(encoding="utf-8"),
        "VAULT_AUDIT_SIEM_CA_CERT": fixture["tls"]["ca_cert"].read_text(encoding="utf-8"),
        **(object_lock_values or object_lock_environment(root)),
    }


class VaultAuditSiemDeliveryGateTest(unittest.TestCase):
    def _prepare_secret_inputs(
        self,
        root: pathlib.Path,
        fixture: dict[str, Any],
        *,
        object_lock_values: dict[str, str] | None = None,
    ) -> tuple[gate.OperationsPolicy, gate.GateOptions, SecretRedactor, gate.SecretInputs]:
        options = gate_options(root / "output", vault_command=("/usr/bin/false",))
        policy = gate.load_operations_policy(POLICY_PATH)
        redactor = SecretRedactor()
        state_dir = gate.ensure_private_directory(root / "state")
        with mock.patch.dict(
            gate.os.environ,
            gate_environment(root, fixture, object_lock_values=object_lock_values),
            clear=True,
        ):
            secret_inputs = gate.prepare_secret_inputs(
                policy,
                options,
                state_dir=state_dir,
                redactor=redactor,
            )
        return policy, options, redactor, secret_inputs

    def test_operations_policy_pins_independent_writer_and_verifier_policies(self) -> None:
        policy = gate.load_operations_policy(POLICY_PATH)

        self.assertEqual(
            policy.object_lock_credential_policy_path.name,
            "audit-object-lock-writer-policy.json",
        )
        self.assertEqual(
            policy.object_lock_verifier_credential_policy_path.name,
            "audit-object-lock-verifier-policy.json",
        )
        self.assertRegex(policy.object_lock_credential_policy_sha256, r"^[0-9a-f]{64}$")
        self.assertRegex(
            policy.object_lock_verifier_credential_policy_sha256,
            r"^[0-9a-f]{64}$",
        )
        self.assertNotEqual(
            policy.object_lock_credential_policy_sha256,
            policy.object_lock_verifier_credential_policy_sha256,
        )

    def _write_verifier_policy_fixture(
        self,
        root: pathlib.Path,
    ) -> tuple[pathlib.Path, dict[str, Any], dict[str, Any], dict[str, Any], pathlib.Path]:
        vault_dir = root / "deploy" / "kubernetes" / "security" / "vault"
        vault_dir.mkdir(parents=True)
        operations_policy = json.loads(POLICY_PATH.read_text(encoding="utf-8"))
        writer_policy = json.loads(
            (
                REPO_ROOT / "deploy/kubernetes/security/vault/audit-object-lock-writer-policy.json"
            ).read_text(encoding="utf-8")
        )
        verifier_policy = json.loads(
            (
                REPO_ROOT
                / "deploy/kubernetes/security/vault/audit-object-lock-verifier-policy.json"
            ).read_text(encoding="utf-8")
        )
        return (
            vault_dir,
            operations_policy,
            writer_policy,
            verifier_policy,
            vault_dir / "operations-policy.json",
        )

    def _assert_verifier_policy_contract_invalid(
        self,
        *,
        mutate_operations_policy: Callable[[dict[str, Any]], None] | None = None,
        mutate_verifier_policy: Callable[[dict[str, Any]], None] | None = None,
        write_verifier_policy: bool = True,
        expected_message: str,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (
                vault_dir,
                operations_policy,
                writer_policy,
                verifier_policy,
                policy_path,
            ) = self._write_verifier_policy_fixture(root)
            if mutate_operations_policy is not None:
                mutate_operations_policy(operations_policy)
            if mutate_verifier_policy is not None:
                mutate_verifier_policy(verifier_policy)
            policy_path.write_text(json.dumps(operations_policy), encoding="utf-8")
            (vault_dir / "audit-object-lock-writer-policy.json").write_text(
                json.dumps(writer_policy),
                encoding="utf-8",
            )
            if write_verifier_policy:
                (vault_dir / "audit-object-lock-verifier-policy.json").write_text(
                    json.dumps(verifier_policy),
                    encoding="utf-8",
                )

            with self.assertRaises(common.ReleaseGateError) as caught:
                gate.load_operations_policy(policy_path)

        self.assertEqual(
            caught.exception.code,
            "release.vault_audit_siem_policy_invalid",
        )
        self.assertIn("verifierCredentialPolicyPath", caught.exception.message)
        self.assertIn(expected_message, caught.exception.message)

    def test_operations_policy_rejects_missing_verifier_policy_path(self) -> None:
        self._assert_verifier_policy_contract_invalid(
            mutate_operations_policy=lambda operations_policy: operations_policy["audit"][
                "externalSiem"
            ]["objectLock"].pop("verifierCredentialPolicyPath"),
            expected_message="was not a non-empty string",
        )

    def test_operations_policy_rejects_missing_verifier_policy_file(self) -> None:
        self._assert_verifier_policy_contract_invalid(
            write_verifier_policy=False,
            expected_message="file was unavailable or malformed",
        )

    def test_operations_policy_rejects_verifier_policy_bucket_drift(self) -> None:
        self._assert_verifier_policy_contract_invalid(
            mutate_verifier_policy=lambda verifier_policy: verifier_policy["Statement"][0].__setitem__(
                "Resource",
                "arn:aws:s3:::other-audit-bucket",
            ),
            expected_message="scoped bucket boundary",
        )

    def test_operations_policy_rejects_verifier_policy_action_allow_list_drift(self) -> None:
        self._assert_verifier_policy_contract_invalid(
            mutate_verifier_policy=lambda verifier_policy: verifier_policy["Statement"][1][
                "Action"
            ].append("s3:PutObject"),
            expected_message="scoped bucket boundary",
        )

    def _run_gate(
        self,
        request_id: str,
        *,
        retention_days: int = 365,
        archive_client: FixtureObjectLockClient | None = None,
        build_clients: Any = None,
        object_lock_values: dict[str, str] | None = None,
        post_receipt: bool = True,
        include_runtime_stub: bool = False,
        repository_state: Callable[[pathlib.Path], dict[str, Any]] | None = None,
    ) -> tuple[int, dict[str, Any], str]:
        with running_sink(retention_days=retention_days, object_lock_client=archive_client) as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            vault_wrapper = write_vault_wrapper(root / "vault-wrapper.py", request_id)
            kubectl_bin = "kubectl"
            if include_runtime_stub:
                kubectl_bin = str(write_kubectl_stub(root / "kubectl.py", request_id))
            output_dir = root / "output"
            options = gate_options(
                output_dir,
                vault_command=(str(vault_wrapper),),
                kubectl_bin=kubectl_bin,
            )
            poster = (
                start_async_receipt_post(fixture["base_url"], fixture["tls"], request_id)
                if post_receipt
                else None
            )
            if repository_state is None:
                repository_state = lambda _repo_root: {"gitSha": "a" * 40, "worktreeDirty": False}
            with contextlib.ExitStack() as stack:
                stack.enter_context(
                    mock.patch.dict(
                        gate.os.environ,
                        gate_environment(root, fixture, object_lock_values=object_lock_values),
                        clear=True,
                    )
                )
                if build_clients is not None:
                    patch_kwargs = {"side_effect": build_clients} if callable(build_clients) else {"return_value": build_clients}
                    stack.enter_context(mock.patch.object(gate, "build_object_lock_clients", **patch_kwargs))
                exit_code = gate.run_vault_audit_siem_delivery_gate(
                    options,
                    repository_state=repository_state,
                )
            if poster is not None:
                poster.join(timeout=5.0)
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))
            markdown = (output_dir / gate.MARKDOWN_REPORT_NAME).read_text(encoding="utf-8")
            return exit_code, report, markdown

    @contextlib.contextmanager
    def _archive_context(
        self,
        request_id: str,
        *,
        retention_days: int = 365,
        object_lock_values: dict[str, str] | None = None,
    ) -> Any:
        store = SharedObjectLockStore()
        archive_client = FixtureObjectLockClient(store, retention_days=retention_days)
        with running_sink(retention_days=retention_days, object_lock_client=archive_client) as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            policy, options, _redactor, secret_inputs = self._prepare_secret_inputs(
                root,
                fixture,
                object_lock_values=object_lock_values,
            )
            poster = start_async_receipt_post(fixture["base_url"], fixture["tls"], request_id)
            poster.join(timeout=5.0)
            status, payload = https_json_request(
                fixture["base_url"],
                f"/v1/receipts?request_id={request_id}&path=sys/audit",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200, payload)
            yield {
                "store": store,
                "policy": policy,
                "options": options,
                "secret_inputs": secret_inputs,
                "receipt": payload["receipt"],
            }

    def test_gate_success_redacts_secrets_and_cleans_temp_files(self) -> None:
        request_id = "req-gate-success-001"
        store = SharedObjectLockStore()
        exit_code, report, markdown = self._run_gate(
            request_id,
            archive_client=FixtureObjectLockClient(store, retention_days=365),
            build_clients=build_gate_object_lock_clients(store, retention_days=365),
            include_runtime_stub=True,
        )

        self.assertEqual(exit_code, 0, report.get("errors"))
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["vault"]["requestId"], request_id)
        self.assertTrue(report["sink"]["deleteRejected"])
        self.assertTrue(report["sink"]["objectLock"]["writerDeleteBlocked"])
        self.assertEqual(report["sink"]["objectLock"]["writerDeleteDenialKind"], "iam")
        self.assertTrue(report["sink"]["objectLock"]["deleteBlocked"])
        self.assertEqual(report["sink"]["objectLock"]["deleteDenialKind"], "object_lock")
        self.assertTrue(report["sink"]["objectLock"]["shortenRetentionBlocked"])
        self.assertEqual(report["sink"]["objectLock"]["shortenRetentionDenialKind"], "object_lock")
        self.assertEqual(report["runtime"]["status"], "observed")
        self.assertRegex(
            report["policy"]["objectLock"]["verifierCredentialPolicySha256"],
            r"^[0-9a-f]{64}$",
        )
        self.assertEqual(report["cleanup"]["removedFileCount"], 4)
        self.assertTrue(report["cleanup"]["stateDirEmpty"])
        self.assertEqual(report["security"]["outputSecretScan"]["status"], "pass")
        for forbidden in (
            "vault-operator-super-secret",
            "fixture-secret",
            "fixture-verifier-secret",
            "BEGIN PRIVATE KEY",
        ):
            self.assertNotIn(forbidden, json.dumps(report))
            self.assertNotIn(forbidden, markdown)

    def test_prepare_secret_inputs_rejects_missing_writer_host_environment(self) -> None:
        with running_sink() as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            object_lock_values = object_lock_environment(root)
            object_lock_values.pop("VAULT_AUDIT_WORM_MC_HOST")

            with self.assertRaises(common.ReleaseGateError) as caught:
                self._prepare_secret_inputs(root, fixture, object_lock_values=object_lock_values)

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_environment_invalid")
        self.assertIn("writer mc host credential", caught.exception.message)

    def test_prepare_secret_inputs_rejects_missing_verifier_host_environment(self) -> None:
        with running_sink() as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            object_lock_values = object_lock_environment(root)
            object_lock_values.pop("VAULT_AUDIT_WORM_MC_VERIFIER_HOST")

            with self.assertRaises(common.ReleaseGateError) as caught:
                self._prepare_secret_inputs(root, fixture, object_lock_values=object_lock_values)

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_environment_invalid")
        self.assertIn("negative-probe verifier", caught.exception.message)

    def test_prepare_secret_inputs_rejects_same_writer_and_verifier_identity(self) -> None:
        with running_sink() as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            object_lock_values = object_lock_environment(
                root,
                writer_host="https://shared-user:shared-secret@object-lock.example.test",
                verifier_host="https://shared-user:different-secret@object-lock.example.test",
            )

            with self.assertRaises(common.ReleaseGateError) as caught:
                self._prepare_secret_inputs(root, fixture, object_lock_values=object_lock_values)

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_environment_invalid")
        self.assertEqual(
            caught.exception.evidence,
            {
                "writerEnvironment": "VAULT_AUDIT_WORM_MC_HOST",
                "verifierEnvironment": "VAULT_AUDIT_WORM_MC_VERIFIER_HOST",
            },
        )

    def test_prepare_secret_inputs_redacts_decoded_object_lock_credential_components(self) -> None:
        with running_sink() as fixture, tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            object_lock_values = object_lock_environment(
                root,
                writer_host="https://fixture%2Bwriter:p%40ss%2Fword@object-lock.example.test",
                verifier_host="https://verify%20reader:v%40erify%2Fsecret@object-lock.example.test",
            )

            _policy, _options, redactor, _secret_inputs = self._prepare_secret_inputs(
                root,
                fixture,
                object_lock_values=object_lock_values,
            )

        redacted = redactor.text(
            "writer fixture+writer p@ss/word verifier verify reader v@erify/secret"
        )
        self.assertNotIn("fixture+writer", redacted)
        self.assertNotIn("p@ss/word", redacted)
        self.assertNotIn("verify reader", redacted)
        self.assertNotIn("v@erify/secret", redacted)
        self.assertIn("[REDACTED_VAULT_AUDIT_WORM_MC_HOST_USERNAME]", redacted)
        self.assertIn("[REDACTED_VAULT_AUDIT_WORM_MC_VERIFIER_HOST_PASSWORD]", redacted)

    def test_gate_fails_when_writer_delete_uses_object_lock_denial_instead_of_iam(self) -> None:
        request_id = "req-gate-writer-denial-001"
        store = SharedObjectLockStore()
        exit_code, report, _markdown = self._run_gate(
            request_id,
            archive_client=FixtureObjectLockClient(store, retention_days=365),
            build_clients=lambda *_args, **_kwargs: build_gate_object_lock_clients(
                store,
                retention_days=365,
                writer_delete_denial_kind="object_lock",
            ),
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["errors"][0]["code"], "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("IAM as required", report["errors"][0]["message"])

    def test_gate_fails_when_verifier_delete_uses_iam_denial_instead_of_object_lock(self) -> None:
        request_id = "req-gate-verifier-delete-001"
        store = SharedObjectLockStore()
        exit_code, report, _markdown = self._run_gate(
            request_id,
            archive_client=FixtureObjectLockClient(store, retention_days=365),
            build_clients=lambda *_args, **_kwargs: build_gate_object_lock_clients(
                store,
                retention_days=365,
                verifier_delete_denial_kind="iam",
            ),
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["errors"][0]["code"], "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("COMPLIANCE/WORM enforcement", report["errors"][0]["message"])

    def test_gate_fails_when_verifier_shorten_retention_uses_iam_denial_instead_of_object_lock(self) -> None:
        request_id = "req-gate-verifier-shorten-001"
        store = SharedObjectLockStore()
        exit_code, report, _markdown = self._run_gate(
            request_id,
            archive_client=FixtureObjectLockClient(store, retention_days=365),
            build_clients=lambda *_args, **_kwargs: build_gate_object_lock_clients(
                store,
                retention_days=365,
                verifier_shorten_denial_kind="iam",
            ),
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["errors"][0]["code"], "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("COMPLIANCE/WORM enforcement", report["errors"][0]["message"])

    def test_verify_sink_chain_uses_healthz_and_requires_verified_ledger_depth(self) -> None:
        secret_inputs = gate.SecretInputs(
            vault_address="https://vault.example.test",
            vault_cacert_value="/tmp/vault-ca.crt",
            vault_cacert_runtime_value="/workspace/vault-ca.crt",
            vault_cacert_environment="VAULT_CACERT",
            auditor_token="vault-token",
            auditor_token_environment="VAULT_OPERATOR_TOKEN",
            sink_endpoint="https://sink.example.test",
            sink_endpoint_environment="SINK_ENDPOINT",
            sink_connect_host="sink.example.test",
            sink_client_cert_path=pathlib.Path("/tmp/client.crt"),
            sink_client_key_path=pathlib.Path("/tmp/client.key"),
            sink_ca_cert_path=pathlib.Path("/tmp/ca.crt"),
            sink_client_certificate_sha256="b" * 64,
            object_lock_alias="worm",
            object_lock_config_dir=pathlib.Path("/tmp/object-lock"),
            object_lock_writer_host="worm-writer.example.test",
            object_lock_verifier_host="worm-verifier.example.test",
            object_lock_resolve=(),
            temporary_paths=(),
        )
        options = gate_options(pathlib.Path("/tmp/output"), vault_command=("vault",))
        with (
            mock.patch.object(gate, "build_sink_ssl_context", return_value=object()),
            mock.patch.object(
                gate,
                "_https_json_request",
                return_value=(
                    200,
                    {
                        "status": "ok",
                        "ledger": {
                            "entryCount": 3,
                            "latestEntrySha256": "a" * 64,
                            "verified": True,
                        },
                    },
                    {"peerCertificateSha256": "c" * 64},
                ),
            ) as request,
        ):
            report = gate.verify_sink_chain(
                sink_endpoint="https://sink.example.test",
                required_ledger_index=2,
                secret_inputs=secret_inputs,
                options=options,
            )

        self.assertEqual(request.call_args.kwargs["url"], "https://sink.example.test/healthz")
        self.assertEqual(report["entryCount"], 3)
        self.assertEqual(report["latestEntrySha256"], "a" * 64)
        self.assertTrue(report["verified"])

    def test_verify_sink_chain_rejects_unverified_or_short_healthz_ledger(self) -> None:
        secret_inputs = gate.SecretInputs(
            vault_address="https://vault.example.test",
            vault_cacert_value="/tmp/vault-ca.crt",
            vault_cacert_runtime_value="/workspace/vault-ca.crt",
            vault_cacert_environment="VAULT_CACERT",
            auditor_token="vault-token",
            auditor_token_environment="VAULT_OPERATOR_TOKEN",
            sink_endpoint="https://sink.example.test",
            sink_endpoint_environment="SINK_ENDPOINT",
            sink_connect_host="sink.example.test",
            sink_client_cert_path=pathlib.Path("/tmp/client.crt"),
            sink_client_key_path=pathlib.Path("/tmp/client.key"),
            sink_ca_cert_path=pathlib.Path("/tmp/ca.crt"),
            sink_client_certificate_sha256="b" * 64,
            object_lock_alias="worm",
            object_lock_config_dir=pathlib.Path("/tmp/object-lock"),
            object_lock_writer_host="worm-writer.example.test",
            object_lock_verifier_host="worm-verifier.example.test",
            object_lock_resolve=(),
            temporary_paths=(),
        )
        options = gate_options(pathlib.Path("/tmp/output"), vault_command=("vault",))
        payloads = (
            {
                "status": "ok",
                "ledger": {
                    "entryCount": 3,
                    "latestEntrySha256": "a" * 64,
                    "verified": False,
                },
            },
            {
                "status": "ok",
                "ledger": {
                    "entryCount": 1,
                    "latestEntrySha256": "a" * 64,
                    "verified": True,
                },
            },
        )
        for payload in payloads:
            with self.subTest(payload=payload):
                with (
                    mock.patch.object(gate, "build_sink_ssl_context", return_value=object()),
                    mock.patch.object(
                        gate,
                        "_https_json_request",
                        return_value=(200, payload, {"peerCertificateSha256": "c" * 64}),
                    ),
                    self.assertRaises(common.ReleaseGateError) as caught,
                ):
                    gate.verify_sink_chain(
                        sink_endpoint="https://sink.example.test",
                        required_ledger_index=2,
                        secret_inputs=secret_inputs,
                        options=options,
                    )

                self.assertEqual(caught.exception.code, "release.vault_audit_siem_chain_invalid")

    def test_verify_object_lock_archive_rejects_payload_hash_tampering(self) -> None:
        request_id = "req-gate-payload-tamper-001"
        with self._archive_context(request_id) as context:
            context["store"].rewrite_latest_entries(
                lambda entries: (
                    entries[0]["payload"]["request"].__setitem__("path", "sys/tampered"),
                    entries[0].__setitem__("entrySha256", gate._canonical_entry_sha256(entries[0])),
                )
            )
            receipt = json.loads(json.dumps(context["receipt"]))
            receipt["archive"]["batchContentSha256"] = context["store"].get(
                context["store"].only_archive_object_key()
            )["contentSha256"]

            with (
                mock.patch.object(
                    gate,
                    "build_object_lock_clients",
                    return_value=build_gate_object_lock_clients(
                        context["store"],
                        retention_days=365,
                    ),
                ),
                self.assertRaises(common.ReleaseGateError) as caught,
            ):
                gate.verify_object_lock_archive(
                    receipt=receipt,
                    policy=context["policy"],
                    secret_inputs=context["secret_inputs"],
                    options=context["options"],
                )

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("payload hash", caught.exception.message)

    def test_verify_object_lock_archive_rejects_receipt_payload_hash_tampering(self) -> None:
        request_id = "req-gate-receipt-payload-tamper-001"
        with self._archive_context(request_id) as context:
            receipt = json.loads(json.dumps(context["receipt"]))
            receipt["payloadSha256"] = "0" * 64

            with (
                mock.patch.object(
                    gate,
                    "build_object_lock_clients",
                    return_value=build_gate_object_lock_clients(
                        context["store"],
                        retention_days=365,
                    ),
                ),
                self.assertRaises(common.ReleaseGateError) as caught,
            ):
                gate.verify_object_lock_archive(
                    receipt=receipt,
                    policy=context["policy"],
                    secret_inputs=context["secret_inputs"],
                    options=context["options"],
                )

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("payload hash", caught.exception.message)

    def test_verify_object_lock_archive_rejects_entry_hash_tampering(self) -> None:
        request_id = "req-gate-entry-tamper-001"
        with self._archive_context(request_id) as context:
            context["store"].rewrite_latest_entries(
                lambda entries: entries[0]["audit"].__setitem__("path", "sys/tampered")
            )
            receipt = json.loads(json.dumps(context["receipt"]))
            receipt["archive"]["batchContentSha256"] = context["store"].get(
                context["store"].only_archive_object_key()
            )["contentSha256"]

            with (
                mock.patch.object(
                    gate,
                    "build_object_lock_clients",
                    return_value=build_gate_object_lock_clients(
                        context["store"],
                        retention_days=365,
                    ),
                ),
                self.assertRaises(common.ReleaseGateError) as caught,
            ):
                gate.verify_object_lock_archive(
                    receipt=receipt,
                    policy=context["policy"],
                    secret_inputs=context["secret_inputs"],
                    options=context["options"],
                )

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("entry hash", caught.exception.message)

    def test_verify_object_lock_archive_requires_structured_payload(self) -> None:
        request_id = "req-gate-payload-required-001"
        with self._archive_context(request_id) as context:
            context["store"].rewrite_latest_entries(
                lambda entries: (
                    entries[0].pop("payload", None),
                    entries[0].__setitem__("entrySha256", gate._canonical_entry_sha256(entries[0])),
                )
            )
            receipt = json.loads(json.dumps(context["receipt"]))
            receipt["archive"]["batchContentSha256"] = context["store"].get(
                context["store"].only_archive_object_key()
            )["contentSha256"]

            with (
                mock.patch.object(
                    gate,
                    "build_object_lock_clients",
                    return_value=build_gate_object_lock_clients(
                        context["store"],
                        retention_days=365,
                    ),
                ),
                self.assertRaises(common.ReleaseGateError) as caught,
            ):
                gate.verify_object_lock_archive(
                    receipt=receipt,
                    policy=context["policy"],
                    secret_inputs=context["secret_inputs"],
                    options=context["options"],
                )

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("archive entry payload", caught.exception.message)

    def test_verify_object_lock_archive_rejects_batch_count_tampering(self) -> None:
        request_id = "req-gate-batch-count-tamper-001"
        with self._archive_context(request_id) as context:
            receipt = json.loads(json.dumps(context["receipt"]))
            receipt["archive"]["batchEntryCount"] = 2

            with (
                mock.patch.object(
                    gate,
                    "build_object_lock_clients",
                    return_value=build_gate_object_lock_clients(
                        context["store"],
                        retention_days=365,
                    ),
                ),
                self.assertRaises(common.ReleaseGateError) as caught,
            ):
                gate.verify_object_lock_archive(
                    receipt=receipt,
                    policy=context["policy"],
                    secret_inputs=context["secret_inputs"],
                    options=context["options"],
                )

        self.assertEqual(caught.exception.code, "release.vault_audit_siem_object_lock_invalid")
        self.assertIn("entry count", caught.exception.message)

    def test_gate_fails_when_receipt_never_arrives(self) -> None:
        exit_code, report, _markdown = self._run_gate(
            "req-gate-missing-001",
            post_receipt=False,
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["errors"][0]["code"], "release.vault_audit_siem_receipt_missing")

    def test_gate_rejects_tamper_evident_sink_without_storage_object_lock(self) -> None:
        exit_code, report, _markdown = self._run_gate(
            "req-gate-no-object-lock-001",
            retention_days=365,
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(
            report["errors"][0]["code"],
            "release.vault_audit_siem_retention_invalid",
        )

    def test_gate_fails_dirty_worktree_preflight(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "output"
            options = gate_options(output_dir, vault_command=("/usr/bin/false",))
            exit_code = gate.run_vault_audit_siem_delivery_gate(
                options,
                repository_state=lambda _repo_root: (_ for _ in ()).throw(
                    common.ReleaseGateError(
                        "release.worktree_dirty",
                        "The consolidated release gate requires a clean worktree with no untracked files.",
                    )
                ),
            )

            self.assertEqual(exit_code, 1)
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "fail")
            self.assertEqual(report["errors"][0]["code"], "release.worktree_dirty")


if __name__ == "__main__":
    unittest.main()
