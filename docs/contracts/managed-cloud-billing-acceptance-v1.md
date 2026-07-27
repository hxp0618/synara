# Managed-cloud billing acceptance v1

> **Deferred / non-blocking.** Synara currently supports self-hosted Kubernetes with operator-managed tariffs and
> requested-resource accounting. Native AWS/GCP/Azure billing exports and cloud Workload Identity are not part of the
> supported Stage 4 product boundary. This document is retained only as a future integration contract; none of its E4/E5
> gates block the current roadmap or release.

This contract defines the evidence that would be required in a future release to promote cloud-billing support from repository or
local-environment verification to a real managed-cloud acceptance claim. It complements
`cloud-cost-accounting-v1.md`; it does not relax that contract's parsing, immutable-object, idempotency, audit, or
reconciliation rules.

The acceptance boundary is intentionally provider-specific. AWS, GCP, and Azure each have an independent gate. A
pass for one provider cannot satisfy another provider's gate, and multiple local or emulated passes cannot be
combined into a managed-cloud pass.

## Evidence levels

Every report declares exactly one evidence level:

| Level | Name                   | Meaning                                                                                                                                  | Eligible for managed-cloud pass         |
| ----- | ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| `E0`  | design/static          | Contract, schema, or source inspection only.                                                                                             | No                                      |
| `E1`  | repository-verified    | Focused tests exercise parsers, normalization, imports, and fail-closed behavior.                                                        | No                                      |
| `E2`  | emulated               | Local files, fixtures, MinIO, fake SDKs, fake metadata services, or another provider-compatible emulator are used.                       | No                                      |
| `E3`  | local-runtime          | Real PostgreSQL, local Kubernetes/Kind/OrbStack, real kubelet, versioned local object storage, or local workload-token plumbing is used. | No                                      |
| `E4`  | managed-cloud-provider | One complete AWS, GCP, or Azure gate runs against that provider's managed identity and real billing-export delivery.                     | Yes, for that provider only             |
| `E5`  | release-accepted       | All provider gates claimed by the release are current, complete, and bound to the exact released artifacts.                              | Yes, only for the declared provider set |

Evidence never upgrades itself. In particular:

- OrbStack, Kind, MinIO, Azurite, fake GCS/S3 services, local metadata servers, synthetic CUR/Cloud Billing/Cost
  Management files, and manually uploaded provider-shaped fixtures remain `E2` or `E3`.
- A local run setting `cloudWorkloadIdentityVerified=true`, using provider-shaped field names, or exercising a real
  SDK does not make it `E4`.
- A real cloud bucket or container read performed with a developer login, static key, node identity, or other
  unbound credential chain is not a managed-cloud workload-identity pass.
- A sanitized copy of a real export loses managed export-delivery provenance unless the report retains a verifiable
  chain from the provider's export job to the exact object version read by Synara.
- A dirty worktree, mutable image tag without its resolved digest, skipped child gate, missing negative assertion, or
  redacted-away identity result may be retained as diagnostic evidence but cannot pass `E4` or `E5`.

## Common managed-cloud gate

An `E4` provider gate runs the production billing adapter and durable import path from a deployed, immutable Synara
artifact. It must bind the report to:

- provider and cloud partition;
- cloud account, project, or subscription and tenant as applicable;
- region or location;
- Kubernetes cluster identity, Namespace, bound ServiceAccount name and UID, Pod name and UID;
- clean source revision, built image digest, and the image digest observed in the running Pod;
- runtime billing mapping identity: Synara tenant, provider, external import ID, parser format, and a digest of the
  normalized mapping with secrets excluded;
- one unpredictable run ID and correlation request ID;
- start and finish times from an external or provider-backed clock source;
- exact export definition, export execution/delivery, and immutable object versions;
- every positive, negative, rotation, revocation, import, replay, and cleanup result required below.

The same deployed image, billing mapping, export delivery, and run ID must be used throughout one provider gate.
Evidence from different revisions, images, accounts, clusters, exports, or runs cannot be spliced into a pass.

### Bound ServiceAccount success

The positive reader runs in a fresh Pod using the exact configured Kubernetes ServiceAccount. The provider's
identity API must return an effective principal that matches the expected workload-identity binding, and the
production adapter must read and import the pinned export using that identity.

The report stores a canonical principal summary and its SHA-256 digest. The canonical summary contains only stable,
non-secret identity fields such as provider, account/project/subscription, tenant when applicable, principal type,
principal resource name or ID, Kubernetes issuer/subject, and role or service-account resource. Volatile session
names, token bytes, signatures, and credentials are excluded. The observed summary digest must exactly equal the
operator-approved expected digest frozen before the positive read. Merely matching an account, project, tenant, or
display name is insufficient.

### Unbound ServiceAccount refusal

An otherwise equivalent fresh Pod runs in the same cluster and network path with a different, explicitly unbound
Kubernetes ServiceAccount. It must be refused access to an exact canary object version that the bound Pod can read.
The refusal must be an authentication/authorization failure or absence of workload credentials before any export
bytes are accepted or any invoice import is committed. A missing object, parser error, network outage, timeout,
wrong key, wrong version, or synthetic failure injection does not prove refusal.

The unbound attempt must not resolve to the bound principal, a node/VM identity, a developer identity, or another
ambient principal. Any successful object metadata read, object read, import, or fallback identity fails the gate.

### No static credentials or node-role fallback

The gate must prove all of the following without recording credential values:

- the Pod specification, effective environment variable names, mounted Secret/config sources, projected volumes,
  SDK configuration, and relevant filesystem metadata contain no static provider access key, service-account key,
  client secret, client certificate/private key, developer credential, or shared credentials file;
- only provider-supported, short-lived workload identity token projection and its non-secret selectors are present;
- the SDK reports or provider identity call corroborates the expected workload credential source;
- the cluster node/VM identity has no permission to read the acceptance export location or decrypt its objects;
- the unbound Pod remains denied with the same network, object, and SDK configuration, proving the bound success did
  not fall through to the node/VM role;
- disabling or removing the workload binding makes a fresh bound-SA Pod fail rather than fall back to an ambient
  chain.

Environment-variable absence by itself is not proof: SDKs can read local files, metadata services, token caches,
credential processes, or node identities. Conversely, standard non-secret workload-identity selectors and projected
token-file paths are allowed and must not be mislabeled as static credentials.

### Exact object version and real export provenance

Every object read is version-specific. The configured version, provider response version, provenance version, and
version stored in scheduled-import audit metadata must match exactly:

- AWS: S3 `VersionId` for the native manifest and every CUR/Data Exports chunk;
- GCP: GCS object `generation` for every extracted export object;
- Azure: Blob `versionId` for every Cost Management export object.

ETag, LastModified, metageneration, snapshot time, or object name alone cannot replace the provider's immutable
version identifier. Unversioned/latest reads are forbidden. If the provider or storage policy cannot supply an
immutable version for every consumed object, the gate fails.

Provenance must start at a real provider billing export, not a manually uploaded fixture. It identifies the export
definition, provider-side export execution or delivery, billing scope and period, destination, creation/completion
time, native format, and the exact delivered object set. For each consumed object it records key/name, immutable
version, size, ETag or provider checksum where available, last-modified time, and Synara's content SHA-256. Manifest
and chunk relationships must satisfy `cloud-cost-accounting-v1.md`. The deterministic bundle checksum in provenance,
the durable invoice `SourceChecksum`, and scheduled-import audit metadata must agree.

### Shared actual-invoice allocation

When the release exposes account-level actual allocation for platform-shared Targets, each provider E4 gate must also
exercise Migration `000082` against the same real export delivery. The operator computes
`sourceScopeAttestationSHA256` from a canonical, secret-free manifest binding the approved cloud billing scope,
effective workload principal digest, export definition/execution, complete immutable object provenance, normalized
Synara mapping digest, provider/currency/period, shared Target, settlement decision, and gate run ID. A handwritten,
random, local-fixture, or object-checksum-only digest does not satisfy this step.

The gate must include at least one exact shared resource line and one deliberately unrelated account line. It proves:

- the selected line matches only the intended shared Target and its complete sealed estimate-slice set;
- signed source micros equal the sum of actual charge slices for every selected line and the whole Run;
- the unrelated line remains in explicit unallocated count/amount rather than being assigned by inference;
- two concurrent first requests return one sealed Run/Line/Slice graph and one mutation audit;
- exact replay returns the retained identity, while a different scope attestation conflicts;
- late invoice-line insertion, late matching estimate-slice insertion, incomplete seal, mutation, delete, and a
  cross-Target ambiguous resource key all fail closed.

The provider report records the allocation Run ID, algorithm, source/line-set/scope-attestation digests, selected and
unallocated counts and signed amounts, slice count, conservation result, replay identity, audit identity, and every
negative result. If this sub-gate is omitted, the provider may still pass basic managed-cloud invoice import, but the
release must declare `sharedActualAllocation=false` and cannot claim managed account-level shared-cost allocation.

Transforms are allowed only when they are an explicit production step. The report must then bind the native export
delivery to the transformation job identity, immutable input snapshot/versions, transformation definition digest,
job completion, and exact output versions. A copied or transformed object without this complete chain is synthetic
for acceptance purposes.

### Rotation and revocation

Each provider gate uses fresh sessions and proves this ordered sequence:

1. The original bound ServiceAccount resolves to the approved old principal and successfully reads the pinned
   object version.
2. Its workload trust or object authorization is revoked. The gate waits within a recorded finite propagation and
   credential-expiry budget; deleting only the Pod is not revocation.
3. A fresh Pod and fresh SDK session using the old binding are denied the same exact object version. Cached object
   bytes, cached credentials, an already-open stream, and an old successful identity response cannot satisfy this
   step.
4. A new workload binding or rotated principal is activated. Its canonical principal summary must match a separately
   pre-approved new digest and must differ from the old digest.
5. A fresh Pod using the new binding successfully reads the same exact object version and replays the same import
   idempotently: it returns the original durable import and line identities and creates no duplicate import audit.
6. The old binding remains denied after the new success.

If the provider cannot revoke an issued short-lived token immediately, the gate may wait for its provider-stated
absolute expiry or apply an explicit deny at the export resource. The report records which mechanism was used and
its timestamps. Exceeding the declared budget, observing an ambiguous result, or restoring access to the old
principal fails closed.

## Provider gates

### AWS gate

The AWS gate requires real EKS workload identity using either IRSA or EKS Pod Identity and a real AWS CUR 2.0/Data
Exports delivery in versioned S3.

Required evidence includes:

- EKS cluster ARN, OIDC issuer or Pod Identity association, Namespace, Kubernetes ServiceAccount subject, IAM role
  ARN and stable role/principal ID, with an exact expected-principal digest;
- successful `GetCallerIdentity` and versioned S3 reads by the bound Pod;
- denial for the unbound ServiceAccount and for the EKS node IAM role against the exact bucket/key/VersionId and KMS
  key when applicable;
- absence of long-lived AWS access keys, shared credentials/config profiles, credential-process output, developer
  SSO cache, and unintended ECS/EC2 metadata fallback; projected web-identity or Pod Identity credentials are the
  allowed source;
- the real export definition ARN/name, export or delivery execution identity, account and billing period, S3 bucket
  ARN/region/prefix, pinned manifest VersionId, and every pinned chunk VersionId;
- the complete native manifest/chunk provenance, including the negative CUR 2.0 boundaries required by the cloud
  cost contract where the real delivery contains the applicable row families;
- rotation or replacement of the workload role/association or its S3/KMS authorization, denial of the old
  principal, success of the approved new principal, and idempotent replay.

An S3-compatible endpoint, MinIO VersionId, local web-identity token, manually uploaded CUR-shaped object, or access
through the node instance profile remains `E2`/`E3`.

### GCP gate

The GCP gate requires real GKE Workload Identity Federation for GKE and a real Cloud Billing export provenance chain.
When the supported JSON adapter consumes an extraction from the native BigQuery billing export, that extraction is a
production transform and must retain the full native-to-GCS chain described above.

Required evidence includes:

- GKE cluster resource, workload identity pool/issuer, Namespace, Kubernetes ServiceAccount subject, mapped IAM
  service account or principal resource, project number, and exact expected-principal digest;
- successful provider identity/token introspection and generation-specific GCS reads by the bound Pod;
- denial for the unbound ServiceAccount and for the GKE node service account against the exact bucket/object/generation
  and Cloud KMS key when applicable;
- absence of service-account JSON keys, `GOOGLE_APPLICATION_CREDENTIALS` key files, gcloud user credentials,
  external static subject tokens, and unintended Compute Engine metadata fallback; GKE's projected/federated
  workload token flow is the allowed source;
- native Cloud Billing BigQuery export dataset/table and partition or snapshot, billing account and period, export
  freshness, extraction/query job ID, normalized query or transformation digest, destination GCS object, and exact
  generation for every consumed object;
- rotation of the Workload Identity IAM binding or mapped principal, denial of the old principal, success of the
  approved new principal, and idempotent replay.

A fake GCS server, local ADC file, manually uploaded Cloud-Billing-shaped JSON, or node-service-account access remains
`E2`/`E3`.

### Azure gate

The Azure gate requires AKS Workload Identity and a real Azure Cost Management export delivery in a versioned Storage
account container.

Required evidence includes:

- AKS cluster resource ID and OIDC issuer, tenant ID, Namespace, Kubernetes ServiceAccount subject, federated
  identity credential ID, managed identity/application client and object IDs, and exact expected-principal digest;
- successful Azure identity evidence and version-specific Blob reads by the bound Pod;
- denial for the unbound ServiceAccount and for the AKS node/kubelet managed identity against the exact storage
  account/container/blob/versionId and Key Vault/CMK key when applicable;
- absence of client secrets, client certificates/private keys, connection strings, shared account keys, SAS tokens,
  Azure CLI/developer caches, and unintended IMDS managed-identity fallback; the projected federated token flow is
  the allowed source;
- Cost Management export definition resource ID, scope, run/execution identity, billing period, destination storage
  account/container/path, completion time, native format, and every exact Blob versionId consumed;
- rotation of the federated identity credential, managed identity, or data-plane authorization, denial of the old
  principal, success of the approved new principal, and idempotent replay.

Azurite, an HTTP emulator endpoint, manually uploaded Cost-Export-shaped CSV/JSON, SAS/account-key access, or AKS
node-identity access remains `E2`/`E3`.

## Evidence document and redaction

The machine-readable report uses `schemaVersion = synara.managed-cloud-billing-acceptance.v1` and contains:

- `status`: `passed` or `failed`;
- `evidenceLevel`: `E0` through `E5`;
- `provider`: `aws`, `gcp`, `azure`, or `aggregate` for `E5` only;
- immutable revision, image, environment, Kubernetes, mapping, export, object-version, and run identities;
- named results for bound success, expected-principal match, unbound refusal, static-credential absence, node/VM
  fallback refusal, exact-version reads, export provenance, durable import, checksum-identical replay, rotation,
  revocation, old-principal recheck, optional shared actual allocation with its explicit release capability flag, and
  cleanup;
- timestamps, bounded retry/propagation budgets, stable error codes, counts, SHA-256 digests, and references to
  separately retained provider audit evidence.

Credential values, projected tokens, authorization headers, signed URLs, connection strings, private keys, and raw
provider responses containing secrets must never enter the report. Redaction cannot remove the stable identity,
version, provenance, or denial fields required to decide the gate. Store canonical principal summaries when their
fields are non-secret; otherwise store the approved and observed summary digests plus a field-name/type manifest that
allows the equality check to be audited.

Every named gate is required. Missing, skipped, stale, malformed, duplicated, out-of-order, cross-run, or ambiguous
evidence makes the provider report `failed`. Cleanup failure also fails the report, but cleanup success cannot repair
an earlier gate failure.

## Aggregation and release use

An `E4` pass is scoped to exactly one provider. An `E5` report may aggregate provider reports only when:

- every provider advertised as managed-cloud billing support by the release has its own passing `E4` report;
- the required provider set is declared before aggregation, and no absent provider is silently removed;
- all child reports use the exact released source revision and deployed image digest;
- every child report is within a declared evidence-age limit of at most 30 days at release decision time;
- child evidence hashes are pinned and the aggregate revalidates every child schema, status, provider, revision,
  image, and required-gate set;
- no child uses `E0`-`E3` evidence as a substitute and no provider pass substitutes for another.

If only one provider is released, the release claim must name that provider rather than saying "managed-cloud
billing" without qualification. Adding another provider requires that provider's independent `E4` gate.

## Non-claims

This contract does not make repository tests or local PostgreSQL/MinIO/OrbStack results less valuable; they remain
required lower-level evidence for deterministic parser, transaction, migration, and failure-path behavior. It only
prevents those results from being mislabeled as proof of managed identity, provider authorization boundaries, native
billing-export delivery, immutable cloud object semantics, or credential rotation/revocation in AWS, GCP, or Azure.
