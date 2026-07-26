# Stage 3 Provider Fixture Acceptance

> **Security limitation / superseded evidence:** this run proved the new install-time exact Worker readiness boundary on
> a disposable OrbStack Ubuntu VM, but an immediate post-run review found that the product SSH `revoke` path did not yet
> revoke already-issued Control Plane Worker/Lease authority and that Target lifecycle transitions still had concurrency
> races. Therefore `Status: pass` applies only to the listed fixture cases and must not be used as SSH revocation or Stage 4
> release evidence. A corrected rerun is required after those defects are fixed.

- Schema: `synara.provider-acceptance.v1`
- Run: `stage3-provider-acceptance-f98febfd-6395-4212-8200-6627b4e0afb8`
- Mode: `fixture`
- Target: `ssh`
- Provider: `codex`
- Status: **pass**
- Started: `2026-07-25T18:56:42.494Z`
- Finished: `2026-07-25T18:58:52.820Z`
- Duration: `130325 ms`

## Evidence boundary

This report uses the deterministic Provider Host fixture through the real Control Plane, agentd, Worker Protocol, and selected Target lifecycle. It is not a real Codex App Server or Claude Agent SDK release gate.

## Cases

| Case | Status | Duration | Reason |
| --- | --- | ---: | --- |
| `environment.target-prepare` | pass | 44187 ms |  |
| `environment.control-plane-start` | pass | 1686 ms |  |
| `identity.dev-login` | pass | 6 ms |  |
| `runtime.worker-discovery` | pass | 16654 ms |  |
| `resources.credential-project-session` | pass | 7 ms |  |
| `fixture.text-tool-usage-artifact` | pass | 1824 ms |  |
| `fixture.approval-resolution` | pass | 1579 ms |  |
| `fixture.terminal-large-log` | pass | 1826 ms |  |
| `fixture.user-input-resolution` | pass | 1848 ms |  |
| `fixture.provider-error` | pass | 1317 ms |  |
| `recovery.worker-replacement` | pass | 37083 ms |  |
| `recovery.post-replacement-workspace-turn` | pass | 515 ms |  |
| `recovery.control-plane-restart` | pass | 132 ms |  |
| `fixture.second-turn-continuity` | pass | 1048 ms |  |
| `environment.cleanup` | pass | 0 ms |  |
| `security.output-secret-scan` | pass | 4 ms |  |

## Evidence

### environment.target-prepare

```json
{
  "controlPlane": {
    "binary": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-xntp7ssj/bin/synara-control-plane",
    "build": "completed",
    "durationMs": 1583,
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/control-plane-build.log",
    "resourceOwner": "b248556a6d874847a791"
  },
  "ssh": {
    "agentd": {
      "durationMs": 6279,
      "goarch": "arm64",
      "goos": "linux",
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-xntp7ssj/bin/synara-agentd-linux-arm64",
      "sha256": "77a951b2dfde3e6db1409b267dca9ea4f988a95635ea31190a23aab9b08c2880"
    },
    "algorithm": "ssh-ed25519",
    "controlPlaneCredentialLifecycle": "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after provisioning, and relies on the Control Plane encrypted credential until ssh/revoke",
    "controlPlaneTransport": {
      "description": "runner-owned reverse SSH relay to the local Worker-only proxy",
      "mode": "reverse-ssh-loopback",
      "vmListenHost": "127.0.0.1"
    },
    "credentialSource": "generated under isolated acceptance state",
    "hostKeyFingerprint": "SHA256:khNCVsicVlV2vUaWfAWEZq2IL5KHZQXmvKuwiE9+0fY",
    "initSystem": "systemd",
    "localPrivateKeyPlaintextDeletedAfterProvision": true,
    "machineAddress": "192.168.139.168",
    "machineArch": "arm64",
    "machineImage": "ubuntu:24.04",
    "machineName": "synara-stage4-ready-20260726",
    "nodeVersion": "24.13.1",
    "ownedMachine": true,
    "providerHostFixture": {
      "durationMs": 73,
      "path": "/var/folders/jd/yq6p0jr96rnfywdlyrsxtvf00000gn/T/synara-stage3-provider-acceptance-xntp7ssj/bin/provider-host-fixture.mjs",
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
  "baseUrl": "http://127.0.0.1:64148",
  "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/control-plane-1.log",
  "pid": 87866,
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
        "latencyMs": 3,
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
    "port": 64991,
    "upstreamAddress": "127.0.0.1:64148"
  },
  "workerProxyRelay": {
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/ssh-worker-proxy-relay.log",
    "mode": "reverse-ssh-loopback",
    "readsUserSSHConfiguration": false,
    "upstreamAddress": "127.0.0.1:64991",
    "vmListenHost": "127.0.0.1",
    "vmListenPort": 64992
  }
}
```

### identity.dev-login

```json
{
  "authenticated": true,
  "organization": {
    "id": "518a0be8-caf3-57e1-8b1c-5e453c422ac2",
    "kind": "root",
    "slug": "personal"
  },
  "tenantId": "c83a8230-94cf-59a6-9a83-ebe90adcaac8",
  "userId": "2bbe44a5-4060-51bd-ac38-3e87ec030a55"
}
```

### runtime.worker-discovery

```json
{
  "driverEvidence": {
    "binarySha256": "77a951b2dfde3e6db1409b267dca9ea4f988a95635ea31190a23aab9b08c2880",
    "controlPlaneCredentialLifecycle": "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after provisioning, and relies on the Control Plane encrypted credential until ssh/revoke",
    "controlPlaneTransport": {
      "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/ssh-worker-proxy-relay.log",
      "mode": "reverse-ssh-loopback",
      "readsUserSSHConfiguration": false,
      "upstreamAddress": "127.0.0.1:64991",
      "vmListenHost": "127.0.0.1",
      "vmListenPort": 64992
    },
    "credentialSource": "runner-generated one-time Ed25519 key",
    "hostKeyAlgorithm": "ssh-ed25519",
    "hostKeyFingerprint": "SHA256:khNCVsicVlV2vUaWfAWEZq2IL5KHZQXmvKuwiE9+0fY",
    "hostKeyMismatch": {
      "errorCode": "ssh_connection_failed",
      "rejected": true,
      "targetId": "10a9adfa-af8e-480a-8e5f-1e96817b1da3"
    },
    "installationId": "stage3-provider-acceptance-83b3518f-5a65-4b88-a20d-1fb6a2fe9994",
    "machineAddress": "192.168.139.168",
    "machineName": "synara-stage4-ready-20260726",
    "ownedMachine": true,
    "runtime": "owned-disposable-orbstack",
    "service": {
      "activeState": "active",
      "mainPid": 3062,
      "restartCount": 0,
      "serviceName": "synara-agentd-91f7d39d-0bd8-42bb-a262-7f5e5ae9de85.service",
      "subState": "running",
      "unitFileState": "enabled"
    },
    "workerAllocation": "standing"
  },
  "manifestId": "1a449549-d885-4df6-980e-b0a10bfc30c6",
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
    "id": "91f7d39d-0bd8-42bb-a262-7f5e5ae9de85",
    "kind": "ssh",
    "name": "stage3-ssh-74877eaef469",
    "organizationId": "518a0be8-caf3-57e1-8b1c-5e453c422ac2",
    "status": "active",
    "tenantId": "c83a8230-94cf-59a6-9a83-ebe90adcaac8"
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
    "id": "90c0a7ad-5fe2-422d-b36d-91cdb9317b94",
    "organizationId": "518a0be8-caf3-57e1-8b1c-5e453c422ac2",
    "provider": "codex",
    "version": 1
  },
  "project": {
    "id": "e532341b-186e-4941-9b73-2afe7fded803",
    "organizationId": "518a0be8-caf3-57e1-8b1c-5e453c422ac2",
    "repositoryUrl": null
  },
  "session": {
    "executionTargetId": "91f7d39d-0bd8-42bb-a262-7f5e5ae9de85",
    "id": "ec81f0fd-7c4d-4247-8579-37a507d3b750",
    "lastEventSequence": 1,
    "provider": "codex",
    "providerCredentialId": "90c0a7ad-5fe2-422d-b36d-91cdb9317b94"
  }
}
```

### fixture.text-tool-usage-artifact

```json
{
  "artifact": {
    "contentType": "text/plain",
    "id": "e42b82e4-ebe1-4e66-83fd-b592199f81b0",
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
  "executionId": "0aadcc21-b5c4-4604-bd40-cccb9e94a162",
  "generation": 1,
  "sequenceRange": {
    "count": 14,
    "first": 2,
    "last": 15
  },
  "turnId": "b3f75b4e-85ef-4b97-add9-0ca7589c1f76",
  "workerId": "2b74d5f9-76c6-408f-b32f-586911ef9b4e"
}
```

### fixture.approval-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "31765e12-7a27-4b47-b240-54f58de3f5b4",
  "interactionId": "ff89f277-ad97-4f98-adeb-0eb9d61329f7",
  "requestId": "fixture-approval-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 16,
    "last": 26
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "4e02bbbe-546b-436e-82d7-fb6e00ee91be"
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
  "executionId": "43240346-5970-441d-9f9f-b7c6dc2cbd29",
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
        "id": "fa6d6e34-5a39-4643-8879-8414825ce610",
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
        "id": "f3adac94-bab5-4b4d-ae99-4645e40f7b1a",
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
        "id": "4554c90f-5a69-47b6-9131-0291d40cb1d6",
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
  "turnId": "cee57dd4-191e-4330-8cfa-e304097bd73c"
}
```

### fixture.user-input-resolution

```json
{
  "deliveryStatus": "pending",
  "executionId": "1cbc564b-1dc0-463f-a635-3091ba6611e4",
  "interactionId": "a8884cef-6398-427b-affc-2ab068494e59",
  "requestId": "fixture-user-input-generation-1-1",
  "resolutionStatus": "resolved",
  "sequenceRange": {
    "count": 11,
    "first": 45,
    "last": 55
  },
  "singleTerminal": true,
  "targetTerminal": null,
  "turnId": "45e43b96-e2a2-4ea2-a1b3-4849ca36d80d"
}
```

### fixture.provider-error

```json
{
  "executionId": "c438df86-fe84-4033-98d2-dc462db23613",
  "failureCode": "provider_rate_limited",
  "sequenceRange": {
    "count": 5,
    "first": 56,
    "last": 60
  },
  "turnId": "c60ccde2-06e7-4493-875a-0fb2eb843977"
}
```

### recovery.worker-replacement

```json
{
  "externalHostRestarted": false,
  "hostKeyFingerprint": "SHA256:khNCVsicVlV2vUaWfAWEZq2IL5KHZQXmvKuwiE9+0fY",
  "instanceUidChanged": true,
  "postReplacementManifestId": "318ed968-d21d-40a1-8631-b4eabd0c1499",
  "previousIncarnation": 1,
  "previousMainPid": 3062,
  "remoteFilesystemContinuity": {
    "preservedAcrossReplacement": true,
    "semantics": "persisted remote-filesystem Workspace content; not Workspace Checkpoint restore"
  },
  "replacementIncarnation": 2,
  "replacementMainPid": 3476,
  "replacementWorkerId": "2b74d5f9-76c6-408f-b32f-586911ef9b4e",
  "serviceName": "synara-agentd-91f7d39d-0bd8-42bb-a262-7f5e5ae9de85.service",
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
  "executionId": "192dad14-3ecc-47b1-bb18-ab0b93484f88",
  "generation": 1,
  "semantics": "persisted remote-filesystem Workspace content; not Workspace Checkpoint restore",
  "sequenceRange": {
    "count": 9,
    "first": 61,
    "last": 69
  },
  "turnId": "0020d025-8842-42a8-a4c9-745aa2dbc26d",
  "workerId": "2b74d5f9-76c6-408f-b32f-586911ef9b4e",
  "workspaceEvidence": {
    "artifactContentVerified": true,
    "artifactRelativePath": ".synara-stage3-acceptance/artifact.txt"
  }
}
```

### recovery.control-plane-restart

```json
{
  "baseUrl": "http://127.0.0.1:64148",
  "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/control-plane-2.log",
  "pid": 88160,
  "postRestartManifestId": "318ed968-d21d-40a1-8631-b4eabd0c1499",
  "preRestartSequence": 69,
  "previousPid": 87866,
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
    "port": 64991,
    "upstreamAddress": "127.0.0.1:64148"
  },
  "workerProxyRelay": {
    "log": "/Users/huang/devel/project/huang/business/synara/docs/reports/stage-4-ssh-atomic-ready-acceptance-20260726/logs/ssh-worker-proxy-relay.log",
    "mode": "reverse-ssh-loopback",
    "readsUserSSHConfiguration": false,
    "upstreamAddress": "127.0.0.1:64991",
    "vmListenHost": "127.0.0.1",
    "vmListenPort": 64992
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
  "executionId": "408f9d9f-f60a-4d44-80e1-4a8ae219aa1d",
  "firstGeneration": 1,
  "generation": 1,
  "generationScope": "per-execution",
  "preRestartSequence": 69,
  "preRestartWorkerId": "2b74d5f9-76c6-408f-b32f-586911ef9b4e",
  "sessionSequenceRange": {
    "count": 80,
    "first": 1,
    "last": 80
  },
  "targetWorkerReplaced": true,
  "terminalSequence": 80,
  "turnId": "58690577-a155-40ca-a797-610512d72d74",
  "turnSequenceRange": {
    "count": 11,
    "first": 70,
    "last": 80
  },
  "workerId": "2b74d5f9-76c6-408f-b32f-586911ef9b4e",
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
  "installationId": "stage3-provider-acceptance-83b3518f-5a65-4b88-a20d-1fb6a2fe9994",
  "localKeyMaterialRemoved": true,
  "machineLifecycleCompleted": true,
  "machineName": "synara-stage4-ready-20260726",
  "machinePreservedByRequest": false,
  "machineRemoved": true,
  "operatorIdentitySourcePreserved": false,
  "ownedRuntimeRemoved": true,
  "productRevokeRequested": true,
  "resourceOwner": "b248556a6d874847a791",
  "runtime": "owned-disposable-orbstack",
  "stateRemoved": true,
  "target": "ssh"
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
  "scannedBytes": 186712,
  "scannedFiles": 13,
  "scope": "acceptance JSON, Markdown, text metadata, and redacted logs; binary SQLite/Artifacts excluded"
}
```
