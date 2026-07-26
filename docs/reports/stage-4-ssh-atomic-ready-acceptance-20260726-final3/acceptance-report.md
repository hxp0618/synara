# Stage 3 Provider Fixture Acceptance

> **Intentionally interrupted / not acceptance evidence:** this run was stopped after an independent review found that
> KMS/config-decryption failure could occur before local SSH Worker authority revocation, and that offline bootstrap was
> not yet bound to the current install/upgrade generation and expected instance UID. The runner reported `fail`; exact VM,
> Worker authority, leases, local credentials, and temporary state were nevertheless cleaned successfully.

- Schema: `synara.provider-acceptance.v1`
- Run: `stage3-provider-acceptance-72fa4bc2-03b4-477d-9ab5-f8743d8177e7`
- Mode: `fixture`
- Target: `ssh`
- Provider: `codex`
- Status: **fail**
- Started: `2026-07-25T19:27:04.427Z`
- Finished: `2026-07-25T19:28:28.023Z`
- Duration: `83595 ms`

## Evidence boundary

This report uses the deterministic Provider Host fixture through the real Control Plane, agentd, Worker Protocol, and selected Target lifecycle. It is not a real Codex App Server or Claude Agent SDK release gate.

## Cases

| Case                                   | Status | Duration | Reason             |
| -------------------------------------- | ------ | -------: | ------------------ |
| `environment.target-prepare`           | pass   | 36647 ms |                    |
| `environment.control-plane-start`      | pass   |  1572 ms |                    |
| `identity.dev-login`                   | pass   |     3 ms |                    |
| `runtime.worker-discovery`             | pass   | 16635 ms |                    |
| `resources.credential-project-session` | pass   |     5 ms |                    |
| `fixture.text-tool-usage-artifact`     | pass   |  1286 ms |                    |
| `fixture.approval-resolution`          | pass   |  1581 ms |                    |
| `fixture.terminal-large-log`           | pass   |  1824 ms |                    |
| `fixture.user-input-resolution`        | pass   |  1582 ms |                    |
| `fixture.provider-error`               | pass   |  1310 ms |                    |
| `recovery.worker-replacement`          | fail   | 11706 ms | runner.interrupted |
| `environment.cleanup`                  | pass   |     0 ms |                    |
| `security.output-secret-scan`          | pass   |     3 ms |                    |

## Evidence

### environment.target-prepare

```json
{
  "controlPlane": {
    "binary": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-h1zfpbzo/bin/synara-control-plane",
    "binarySha256": "d48b999ea433f4c6c4c2ed11d90cd4d0733e029cbb22ba647b44b7f64bfa5454",
    "build": "completed",
    "durationMs": 1726,
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final3/logs/control-plane-build.log",
    "resourceOwner": "5c988bc0e8af494e877c"
  },
  "ssh": {
    "agentd": {
      "durationMs": 708,
      "goarch": "arm64",
      "goos": "linux",
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-h1zfpbzo/bin/synara-agentd-linux-arm64",
      "sha256": "4c39ca6b7f99ecaeef40b42b6cbc334cca2bb740f893658e4ac1b8b8d122d800"
    },
    "algorithm": "ssh-ed25519",
    "controlPlaneCredentialLifecycle": "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after provisioning, and relies on the Control Plane encrypted credential until ssh/revoke",
    "controlPlaneTransport": {
      "description": "runner-owned reverse SSH relay to the local Worker-only proxy",
      "mode": "reverse-ssh-loopback",
      "vmListenHost": "127.0.0.1"
    },
    "credentialSource": "generated under isolated acceptance state",
    "hostKeyFingerprint": "SHA256:R/mfzeo0QQrUa6bgSnDM/GvrsHaKQooIzCyC6w7M0vk",
    "initSystem": "systemd",
    "localPrivateKeyPlaintextDeletedAfterProvision": true,
    "machineAddress": "192.168.139.124",
    "machineArch": "arm64",
    "machineImage": "ubuntu:24.04",
    "machineName": "synara-stage4-ready-final3-20260726",
    "nodeVersion": "24.13.1",
    "ownedMachine": true,
    "providerHostFixture": {
      "durationMs": 29,
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-h1zfpbzo/bin/provider-host-fixture.mjs",
      "remotePath": "/opt/synara/acceptance/provider-host-fixture.mjs",
      "runtime": "deterministic-fixture",
      "sha256": "0b25f2eba94ff8833281e9828cf1f3a34205288829876940ee6e5b5c02dea3d4"
    },
    "providerRuntime": {
      "kind": "deterministic-fixture"
    },
    "sshd": "active"
  }
}
```

### environment.control-plane-start

```json
{
  "baseUrl": "http://127.0.0.1:59683",
  "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final3/logs/control-plane-1.log",
  "pid": 1581,
  "processGeneration": 1,
  "readiness": {
    "checks": {
      "artifactStore": {
        "kind": "local",
        "latencyMs": 0,
        "status": "ready"
      },
      "database": {
        "kind": "sqlite",
        "latencyMs": 0,
        "status": "ready"
      },
      "databaseWrite": {
        "kind": "sqlite",
        "latencyMs": 0,
        "status": "ready"
      },
      "queue": {
        "kind": "in-process",
        "latencyMs": 0,
        "status": "ready"
      },
      "schema": {
        "kind": "sqlite",
        "latencyMs": 2,
        "status": "ready"
      }
    },
    "status": "ready"
  },
  "workerProxy": {
    "advertisedHost": "127.0.0.1",
    "allowedPathPrefixes": ["/v1/workers/", "/v1/artifact-content/"],
    "faultInjection": "runner-owned transport close before HTTP forwarding",
    "listenAddress": "0.0.0.0",
    "port": 60196,
    "upstreamAddress": "127.0.0.1:59683"
  },
  "workerProxyRelay": {
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final3/logs/ssh-worker-proxy-relay.log",
    "mode": "reverse-ssh-loopback",
    "readsUserSSHConfiguration": false,
    "upstreamAddress": "127.0.0.1:60196",
    "vmListenHost": "127.0.0.1",
    "vmListenPort": 60197
  }
}
```

### identity.dev-login

```json
{
  "authenticated": true,
  "organization": {
    "id": "d036e38c-59fa-5a90-a81b-61a795ec7b2a",
    "kind": "root",
    "slug": "personal"
  },
  "tenantId": "f2f50479-378a-5d37-ad74-ae0fea05a5f4",
  "userId": "aaa62c13-9cb5-5aed-8ed4-c64d15ef39e3"
}
```

### runtime.worker-discovery

```json
{
  "driverEvidence": {
    "binarySha256": "4c39ca6b7f99ecaeef40b42b6cbc334cca2bb740f893658e4ac1b8b8d122d800",
    "controlPlaneCredentialLifecycle": "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after provisioning, and relies on the Control Plane encrypted credential until ssh/revoke",
    "controlPlaneTransport": {
      "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final3/logs/ssh-worker-proxy-relay.log",
      "mode": "reverse-ssh-loopback",
      "readsUserSSHConfiguration": false,
      "upstreamAddress": "127.0.0.1:60196",
      "vmListenHost": "127.0.0.1",
      "vmListenPort": 60197
    },
    "credentialSource": "runner-generated one-time Ed25519 key",
    "hostKeyAlgorithm": "ssh-ed25519",
    "hostKeyFingerprint": "SHA256:R/mfzeo0QQrUa6bgSnDM/GvrsHaKQooIzCyC6w7M0vk",
    "hostKeyMismatch": {
      "errorCode": "ssh_connection_failed",
      "rejected": true,
      "targetId": "6d2cc76c-ddce-4a7f-8c8f-f30f28a4651a"
    },
    "installationId": "stage3-provider-acceptance-5b1a0f06-4f75-420b-b518-61830992edee",
    "machineAddress": "192.168.139.124",
    "machineName": "synara-stage4-ready-final3-20260726",
    "ownedMachine": true,
    "readyBoundary": {
      "compatibilityStatus": "compatible",
      "creationStatus": "offline",
      "manifestId": "b3168f26-c5f2-4949-a232-ba6ebb937869",
      "operationActive": false,
      "operationGeneration": 1,
      "postRegistrationHeartbeat": true,
      "targetStatus": "active",
      "workerAdministrativeStatus": "active",
      "workerId": "a0c2a039-07a9-4a8a-b2e1-e1baccac3cb7",
      "workerIncarnation": 1,
      "workerInstanceUid": "b0bad6ab-c7f9-41b9-ae59-e5a23a9af32b",
      "workerStatus": "online"
    },
    "runtime": "owned-disposable-orbstack",
    "service": {
      "activeState": "active",
      "mainPid": 3061,
      "restartCount": 0,
      "serviceName": "synara-agentd-13759755-692b-45c7-8f82-57ec548b3de9.service",
      "subState": "running",
      "unitFileState": "enabled"
    },
    "workerAllocation": "standing"
  },
  "manifestId": "b3168f26-c5f2-4949-a232-ba6ebb937869",
  "provider": {
    "compatibilityStatus": "compatible",
    "provider": "codex",
    "releasePolicy": {
      "enabled": true,
      "requiresExplicitEnablement": true
    },
    "runtime": {
      "available": true,
      "compatible": true,
      "compatibleRange": {
        "maximumExclusive": "0.146.0",
        "minimumInclusive": "0.145.0"
      },
      "kind": "cli",
      "name": "codex",
      "version": "0.145.0",
      "versionSource": "probe"
    },
    "supportTier": "experimental"
  },
  "runtimeEvent": {
    "maximum": 2,
    "minimum": 2
  },
  "target": {
    "id": "13759755-692b-45c7-8f82-57ec548b3de9",
    "kind": "ssh",
    "name": "stage3-ssh-6d704a6fe0d9",
    "organizationId": "d036e38c-59fa-5a90-a81b-61a795ec7b2a",
    "status": "active",
    "tenantId": "f2f50479-378a-5d37-ad74-ae0fea05a5f4"
  },
  "workerBuild": {
    "architecture": "arm64",
    "operatingSystem": "linux",
    "version": "managed"
  },
  "workerProtocol": {
    "maximum": 2,
    "minimum": 2
  },
  "workerStatusCounts": {
    "draining": 0,
    "offline": 0,
    "online": 1
  }
}
```

### resources.credential-project-session

```json
{
  "credential": {
    "credentialType": "api_key",
    "delivery": "acceptance-fixture",
    "id": "13ed6b4e-4209-49ae-8383-fb02b7daa979",
    "organizationId": "d036e38c-59fa-5a90-a81b-61a795ec7b2a",
    "provider": "codex",
    "version": 1
  },
  "project": {
    "id": "ea8cc765-6c58-4c4f-9616-a665628a4b31",
    "organizationId": "d036e38c-59fa-5a90-a81b-61a795ec7b2a",
    "repositoryUrl": null
  },
  "session": {
    "executionTargetId": "13759755-692b-45c7-8f82-57ec548b3de9",
    "id": "89ef18a9-3ca5-4a55-ba50-5de0923d8e0b",
    "lastEventSequence": 1,
    "provider": "codex",
    "providerCredentialId": "13ed6b4e-4209-49ae-8383-fb02b7daa979"
  }
}
```

### fixture.text-tool-usage-artifact

```json
{
  "artifact": {
    "contentType": "text/plain",
    "id": "238ed1e5-3bf9-4af9-98b5-179add023e53",
    "kind": "generated_file",
    "originalName": "artifact.txt",
    "sha256": "5da2790ad273f8535991c95abed867ed3e786a0ad2ca22b0fabbbda344d9ff4b",
    "sizeBytes": 42,
    "status": "ready"
  },
  "credentialEvidence": {
    "credentialPayloadKeys": ["apiKey"],
    "credentialVerified": true
  },
  "eventTypes": [
    "turn.created",
    "execution.leased",
    "workspace.ready",
    "execution.started",
    "item.started",
    "item.completed",
    "thread.token-usage.updated",
    "artifact.ready",
    "content.delta",
    "workspace.dirty",
    "checkpoint.created",
    "artifact.ready",
    "checkpoint.ready",
    "execution.completed"
  ],
  "executionId": "55ecf3fd-6ccd-4ed3-97b6-ecfd0ac8a6c1",
  "generation": 1,
  "sequenceRange": {
    "count": 14,
    "first": 2,
    "last": 15
  },
  "turnId": "b523b823-6cc6-4545-809f-633299e3940b",
  "workerId": "a0c2a039-07a9-4a8a-b2e1-e1baccac3cb7"
}
```

### fixture.approval-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "29d92be0-4a34-4ff1-81fe-dd6f889951d9",
  "interactionId": "35cae259-55ea-4c8e-b801-3c9be706d692",
  "requestId": "fixture-approval-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 16,
    "last": 26
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "d4260809-3bb2-4cc3-9f57-6424f0db98d6"
}
```

### fixture.terminal-large-log

```json
{
  "completion": {
    "exitCode": 0,
    "previewBytes": 32768,
    "segmentCount": 3,
    "totalBytes": 2097409,
    "truncated": true
  },
  "executionId": "d696ca8e-3a69-42d7-ba91-00659acd5bbe",
  "preview": {
    "bytes": 32768,
    "eventCount": 1,
    "sha256": "31e153e2aa53cad80b7572b88da87ee3b43f40d1213ab96077754f6c1dcb3e34",
    "truncated": true
  },
  "runtimePhysicalPathLeak": false,
  "segments": [
    {
      "artifact": {
        "contentType": "text/plain; charset=utf-8",
        "id": "4e601b1d-4247-453c-84c2-d3ce9c9d97f9",
        "kind": "terminal_log",
        "originalName": "terminal-log-000001.log",
        "sha256": "f22d03ccbcfd9f40f8a8adb9deaa74e9c4fddc6f0325158a260021c698f0c869",
        "sizeBytes": 1048576,
        "status": "ready"
      },
      "length": 1048576,
      "offset": 0,
      "segmentIndex": 0
    },
    {
      "artifact": {
        "contentType": "text/plain; charset=utf-8",
        "id": "d672c7ba-23d9-422c-ab50-45a71207314f",
        "kind": "terminal_log",
        "originalName": "terminal-log-000002.log",
        "sha256": "eb149a408fa80e2faf39670f5e8e357a61d723d5d5b5d3620a9ca05105b636be",
        "sizeBytes": 1048576,
        "status": "ready"
      },
      "length": 1048576,
      "offset": 1048576,
      "segmentIndex": 1
    },
    {
      "artifact": {
        "contentType": "text/plain; charset=utf-8",
        "id": "58ac28ba-48e7-4350-8644-2244a183aa3e",
        "kind": "terminal_log",
        "originalName": "terminal-log-000003.log",
        "sha256": "5fa2911d4a2a4821ba301f5256983895d62da71a9a5c4e8237e6a8900d4c09c1",
        "sizeBytes": 257,
        "status": "ready"
      },
      "length": 257,
      "offset": 2097152,
      "segmentIndex": 2
    }
  ],
  "sequenceRange": {
    "count": 18,
    "first": 27,
    "last": 44
  },
  "terminalId": "fixture-terminal-large-1",
  "turnId": "112164ad-698b-4386-9516-420cb7e1b28c"
}
```

### fixture.user-input-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "e053442f-ff34-4fdb-8de8-0417e824657b",
  "interactionId": "30c411e8-a7a4-4b79-b95e-0008421627da",
  "requestId": "fixture-user-input-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 45,
    "last": 55
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "653530d2-dd4b-439d-8a68-48af955742f9"
}
```

### fixture.provider-error

```json
{
  "executionId": "86236b7a-ecae-4b5e-beb0-9ccd66396e9e",
  "failureCode": "provider_rate_limited",
  "sequenceRange": {
    "count": 5,
    "first": 56,
    "last": 60
  },
  "turnId": "0dea996a-26dd-4155-a4d7-1af0dfc85fea"
}
```

### recovery.worker-replacement

Acceptance run interrupted by SIGINT.

```json
{
  "signal": "SIGINT",
  "signalNumber": 2
}
```

### environment.cleanup

```json
{
  "broadCleanupUsed": false,
  "externalHostPreserved": false,
  "externalHostRestarted": false,
  "installationId": "stage3-provider-acceptance-5b1a0f06-4f75-420b-b518-61830992edee",
  "localKeyMaterialRemoved": true,
  "machineLifecycleCompleted": true,
  "machineName": "synara-stage4-ready-final3-20260726",
  "machinePreservedByRequest": false,
  "machineRemoved": true,
  "operatorIdentitySourcePreserved": false,
  "ownedRuntimeRemoved": true,
  "productRevokeRequested": true,
  "resourceOwner": "5c988bc0e8af494e877c",
  "runtime": "owned-disposable-orbstack",
  "stateRemoved": true,
  "target": "ssh",
  "workerAuthorityRevocation": {
    "executionLeases": 0,
    "liveWorkerAuthorities": 0,
    "operationActive": false,
    "revokedWorkers": 1,
    "targetStatus": "disabled",
    "totalWorkers": 1,
    "workspaceCleanupLeases": 0
  }
}
```

### security.output-secret-scan

```json
{
  "fileTypes": [".json", ".log", ".md", ".txt", ".yaml", ".yml"],
  "findings": [],
  "knownSecretCount": 11,
  "patternNames": ["private-key-pem", "aws-access-key", "github-token", "openai-style-key"],
  "scannedBytes": 142564,
  "scannedFiles": 12,
  "scope": "acceptance JSON, Markdown, text metadata, and redacted logs; binary SQLite/Artifacts excluded"
}
```
