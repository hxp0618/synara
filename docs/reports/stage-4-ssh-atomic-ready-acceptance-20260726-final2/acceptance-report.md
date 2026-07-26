# Stage 3 Provider Fixture Acceptance

> **Intermediate evidence only:** this run proves the valid-config authority-before-remote revoke path and records zero
> remaining Worker/Lease authority, but a later review found that KMS/config-decryption failure still happened before the
> local revoke fence and that offline bootstrap was not yet bound to the current operation/expected instance UID. Do not
> use this report as final SSH security acceptance; a post-fix rerun is required.

- Schema: `synara.provider-acceptance.v1`
- Run: `stage3-provider-acceptance-b02bffbe-6bb0-4da2-ad63-a9cb0a1b0074`
- Mode: `fixture`
- Target: `ssh`
- Provider: `codex`
- Status: **pass**
- Started: `2026-07-25T19:22:36.278Z`
- Finished: `2026-07-25T19:24:43.359Z`
- Duration: `127080 ms`

## Evidence boundary

This report uses the deterministic Provider Host fixture through the real Control Plane, agentd, Worker Protocol, and selected Target lifecycle. It is not a real Codex App Server or Claude Agent SDK release gate.

## Cases

| Case | Status | Duration | Reason |
| --- | --- | ---: | --- |
| `environment.target-prepare` | pass | 40318 ms |  |
| `environment.control-plane-start` | pass | 1879 ms |  |
| `identity.dev-login` | pass | 6 ms |  |
| `runtime.worker-discovery` | pass | 16524 ms |  |
| `resources.credential-project-session` | pass | 7 ms |  |
| `fixture.text-tool-usage-artifact` | pass | 1557 ms |  |
| `fixture.approval-resolution` | pass | 1590 ms |  |
| `fixture.terminal-large-log` | pass | 1576 ms |  |
| `fixture.user-input-resolution` | pass | 1847 ms |  |
| `fixture.provider-error` | pass | 1578 ms |  |
| `recovery.worker-replacement` | pass | 36910 ms |  |
| `recovery.post-replacement-workspace-turn` | pass | 1572 ms |  |
| `recovery.control-plane-restart` | pass | 150 ms |  |
| `fixture.second-turn-continuity` | pass | 1056 ms |  |
| `environment.cleanup` | pass | 0 ms |  |
| `security.output-secret-scan` | pass | 4 ms |  |

## Evidence

### environment.target-prepare

```json
{
  "controlPlane": {
    "binary": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-2dtmimip/bin/synara-control-plane",
    "binarySha256": "d48b999ea433f4c6c4c2ed11d90cd4d0733e029cbb22ba647b44b7f64bfa5454",
    "build": "completed",
    "durationMs": 1564,
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/control-plane-build.log",
    "resourceOwner": "d7271ec7dfde4606bbb3"
  },
  "ssh": {
    "agentd": {
      "durationMs": 2954,
      "goarch": "arm64",
      "goos": "linux",
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-2dtmimip/bin/synara-agentd-linux-arm64",
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
    "hostKeyFingerprint": "SHA256:UdD/NfaCKzmG8FnUznq4H5Xrv2CIu251day8s4tzBjA",
    "initSystem": "systemd",
    "localPrivateKeyPlaintextDeletedAfterProvision": true,
    "machineAddress": "192.168.139.165",
    "machineArch": "arm64",
    "machineImage": "ubuntu:24.04",
    "machineName": "synara-stage4-ready-final2-20260726",
    "nodeVersion": "24.13.1",
    "ownedMachine": true,
    "providerHostFixture": {
      "durationMs": 69,
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-2dtmimip/bin/provider-host-fixture.mjs",
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
  "baseUrl": "http://127.0.0.1:56286",
  "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/control-plane-1.log",
  "pid": 168,
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
    "allowedPathPrefixes": [
      "/v1/workers/",
      "/v1/artifact-content/"
    ],
    "faultInjection": "runner-owned transport close before HTTP forwarding",
    "listenAddress": "0.0.0.0",
    "port": 56811,
    "upstreamAddress": "127.0.0.1:56286"
  },
  "workerProxyRelay": {
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/ssh-worker-proxy-relay.log",
    "mode": "reverse-ssh-loopback",
    "readsUserSSHConfiguration": false,
    "upstreamAddress": "127.0.0.1:56811",
    "vmListenHost": "127.0.0.1",
    "vmListenPort": 56812
  }
}
```

### identity.dev-login

```json
{
  "authenticated": true,
  "organization": {
    "id": "cc1bfaee-e502-5433-b643-4fdf1212d4ff",
    "kind": "root",
    "slug": "personal"
  },
  "tenantId": "f746a106-4338-5f8c-9655-55e482ee8f81",
  "userId": "5d84e9e2-41f4-50a6-a2b7-2f90220fa398"
}
```

### runtime.worker-discovery

```json
{
  "driverEvidence": {
    "binarySha256": "4c39ca6b7f99ecaeef40b42b6cbc334cca2bb740f893658e4ac1b8b8d122d800",
    "controlPlaneCredentialLifecycle": "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after provisioning, and relies on the Control Plane encrypted credential until ssh/revoke",
    "controlPlaneTransport": {
      "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/ssh-worker-proxy-relay.log",
      "mode": "reverse-ssh-loopback",
      "readsUserSSHConfiguration": false,
      "upstreamAddress": "127.0.0.1:56811",
      "vmListenHost": "127.0.0.1",
      "vmListenPort": 56812
    },
    "credentialSource": "runner-generated one-time Ed25519 key",
    "hostKeyAlgorithm": "ssh-ed25519",
    "hostKeyFingerprint": "SHA256:UdD/NfaCKzmG8FnUznq4H5Xrv2CIu251day8s4tzBjA",
    "hostKeyMismatch": {
      "errorCode": "ssh_connection_failed",
      "rejected": true,
      "targetId": "58ba7c41-2450-4b0a-98d2-9d1da51be887"
    },
    "installationId": "stage3-provider-acceptance-63e38478-afd0-4cd5-9e77-2b15be842916",
    "machineAddress": "192.168.139.165",
    "machineName": "synara-stage4-ready-final2-20260726",
    "ownedMachine": true,
    "runtime": "owned-disposable-orbstack",
    "service": {
      "activeState": "active",
      "mainPid": 3061,
      "restartCount": 0,
      "serviceName": "synara-agentd-b9e36b91-c9b9-45c6-b0ec-9187164429dc.service",
      "subState": "running",
      "unitFileState": "enabled"
    },
    "workerAllocation": "standing"
  },
  "manifestId": "1bd48f36-8c93-4f04-ade6-43168a392b4d",
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
    "id": "b9e36b91-c9b9-45c6-b0ec-9187164429dc",
    "kind": "ssh",
    "name": "stage3-ssh-79cd2f9e64cf",
    "organizationId": "cc1bfaee-e502-5433-b643-4fdf1212d4ff",
    "status": "offline",
    "tenantId": "f746a106-4338-5f8c-9655-55e482ee8f81"
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
    "id": "de3cc81a-cf6c-4b9a-a908-063f53d46828",
    "organizationId": "cc1bfaee-e502-5433-b643-4fdf1212d4ff",
    "provider": "codex",
    "version": 1
  },
  "project": {
    "id": "50707334-c7ea-40c0-9040-ead24d422dae",
    "organizationId": "cc1bfaee-e502-5433-b643-4fdf1212d4ff",
    "repositoryUrl": null
  },
  "session": {
    "executionTargetId": "b9e36b91-c9b9-45c6-b0ec-9187164429dc",
    "id": "bccedeff-4a29-4c6f-944d-ace6e3eea4ca",
    "lastEventSequence": 1,
    "provider": "codex",
    "providerCredentialId": "de3cc81a-cf6c-4b9a-a908-063f53d46828"
  }
}
```

### fixture.text-tool-usage-artifact

```json
{
  "artifact": {
    "contentType": "text/plain",
    "id": "8cd8b3ad-2ccf-4674-bd59-c6cb907eed45",
    "kind": "generated_file",
    "originalName": "artifact.txt",
    "sha256": "5da2790ad273f8535991c95abed867ed3e786a0ad2ca22b0fabbbda344d9ff4b",
    "sizeBytes": 42,
    "status": "ready"
  },
  "credentialEvidence": {
    "credentialPayloadKeys": [
      "apiKey"
    ],
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
  "executionId": "2d84e0ec-9616-43af-835f-64127827968f",
  "generation": 1,
  "sequenceRange": {
    "count": 14,
    "first": 2,
    "last": 15
  },
  "turnId": "dd1b06be-ac00-4b0a-87e6-684341a7203c",
  "workerId": "c33642bc-4484-46ca-bcf3-f3ef496ed35b"
}
```

### fixture.approval-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "89bd3d9e-3159-4e12-8b1f-09f0fe8b9860",
  "interactionId": "c0ce3938-f1fb-40b7-a802-3a7f3e7580c8",
  "requestId": "fixture-approval-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 16,
    "last": 26
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "ad33f326-6bca-43e9-8be4-40e4baf15f03"
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
  "executionId": "16a3a24d-7f83-4284-9cd5-623c989a6a23",
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
        "id": "88099e5e-c658-433e-8888-393fe3df5eb4",
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
        "id": "78a414a3-248f-4a72-a2f2-28758f288f04",
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
        "id": "bdb426bc-b233-4c1b-aed2-7f8d9d5d714f",
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
  "turnId": "8df2c452-3f3f-4f9c-9dc4-5f9c1bcdad01"
}
```

### fixture.user-input-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "0f06be53-27db-43e7-9ea8-4695646c4e4f",
  "interactionId": "cd11f254-4799-4c68-a686-3694aff191e8",
  "requestId": "fixture-user-input-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 45,
    "last": 55
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "edd460f5-79fe-4bc3-b040-38debac3021e"
}
```

### fixture.provider-error

```json
{
  "executionId": "60e307a8-f03f-4b76-bfe5-ff4596ba3318",
  "failureCode": "provider_rate_limited",
  "sequenceRange": {
    "count": 5,
    "first": 56,
    "last": 60
  },
  "turnId": "d57f562e-6f94-4fee-8067-35ab1ab25219"
}
```

### recovery.worker-replacement

```json
{
  "externalHostRestarted": false,
  "hostKeyFingerprint": "SHA256:UdD/NfaCKzmG8FnUznq4H5Xrv2CIu251day8s4tzBjA",
  "instanceUidChanged": true,
  "postReplacementManifestId": "059a6b63-94b6-44b2-a32f-eaff043fb370",
  "previousIncarnation": 1,
  "previousMainPid": 3061,
  "remoteFilesystemContinuity": {
    "preservedAcrossReplacement": true,
    "semantics": "persisted remote-filesystem Workspace content; not Workspace Checkpoint restore"
  },
  "replacementIncarnation": 2,
  "replacementMainPid": 3476,
  "replacementWorkerId": "c33642bc-4484-46ca-bcf3-f3ef496ed35b",
  "serviceName": "synara-agentd-b9e36b91-c9b9-45c6-b0ec-9187164429dc.service",
  "sshdRestarted": true,
  "strategy": "pinned-Host-Key SSH upgrade with systemd restart",
  "workerIdStable": true,
  "workerStatusCounts": {
    "draining": 0,
    "offline": 0,
    "online": 1
  }
}
```

### recovery.post-replacement-workspace-turn

```json
{
  "executionId": "580d2c71-cec5-4fa2-a6b9-a168b336cc05",
  "generation": 1,
  "semantics": "persisted remote-filesystem Workspace content; not Workspace Checkpoint restore",
  "sequenceRange": {
    "count": 9,
    "first": 61,
    "last": 69
  },
  "turnId": "8cb83f97-5180-4c16-a9ef-39968be06961",
  "workerId": "c33642bc-4484-46ca-bcf3-f3ef496ed35b",
  "workspaceEvidence": {
    "artifactContentVerified": true,
    "artifactRelativePath": ".synara-stage3-acceptance/artifact.txt"
  }
}
```

### recovery.control-plane-restart

```json
{
  "baseUrl": "http://127.0.0.1:56286",
  "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/control-plane-2.log",
  "pid": 428,
  "postRestartManifestId": "059a6b63-94b6-44b2-a32f-eaff043fb370",
  "preRestartSequence": 69,
  "previousPid": 168,
  "processGeneration": 2,
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
    "allowedPathPrefixes": [
      "/v1/workers/",
      "/v1/artifact-content/"
    ],
    "faultInjection": "runner-owned transport close before HTTP forwarding",
    "listenAddress": "0.0.0.0",
    "port": 56811,
    "upstreamAddress": "127.0.0.1:56286"
  },
  "workerProxyRelay": {
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726-final2/logs/ssh-worker-proxy-relay.log",
    "mode": "reverse-ssh-loopback",
    "readsUserSSHConfiguration": false,
    "upstreamAddress": "127.0.0.1:56811",
    "vmListenHost": "127.0.0.1",
    "vmListenPort": 56812
  },
  "workerStatusCounts": {
    "draining": 0,
    "offline": 0,
    "online": 1
  }
}
```

### fixture.second-turn-continuity

```json
{
  "executionId": "4596058f-8185-4e84-8e69-d66c8803ba8a",
  "firstGeneration": 1,
  "generation": 1,
  "generationScope": "per-execution",
  "preRestartSequence": 69,
  "preRestartWorkerId": "c33642bc-4484-46ca-bcf3-f3ef496ed35b",
  "sessionSequenceRange": {
    "count": 80,
    "first": 1,
    "last": 80
  },
  "targetWorkerReplaced": true,
  "terminalSequence": 80,
  "turnId": "4c697362-d99b-400a-aaf3-f860f637d5b6",
  "turnSequenceRange": {
    "count": 11,
    "first": 70,
    "last": 80
  },
  "workerId": "c33642bc-4484-46ca-bcf3-f3ef496ed35b",
  "workerIdChangedAfterRestart": false,
  "workerIdSemantics": "stable registration slot; a restarted agentd registration may reuse the Worker ID"
}
```

### environment.cleanup

```json
{
  "broadCleanupUsed": false,
  "externalHostPreserved": false,
  "externalHostRestarted": false,
  "installationId": "stage3-provider-acceptance-63e38478-afd0-4cd5-9e77-2b15be842916",
  "localKeyMaterialRemoved": true,
  "machineLifecycleCompleted": true,
  "machineName": "synara-stage4-ready-final2-20260726",
  "machinePreservedByRequest": false,
  "machineRemoved": true,
  "operatorIdentitySourcePreserved": false,
  "ownedRuntimeRemoved": true,
  "productRevokeRequested": true,
  "resourceOwner": "d7271ec7dfde4606bbb3",
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
  "fileTypes": [
    ".json",
    ".log",
    ".md",
    ".txt",
    ".yaml",
    ".yml"
  ],
  "findings": [],
  "knownSecretCount": 11,
  "patternNames": [
    "private-key-pem",
    "aws-access-key",
    "github-token",
    "openai-style-key"
  ],
  "scannedBytes": 187452,
  "scannedFiles": 13,
  "scope": "acceptance JSON, Markdown, text metadata, and redacted logs; binary SQLite/Artifacts excluded"
}
```
