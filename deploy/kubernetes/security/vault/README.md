# Vault production baseline

These files pin the production Vault deployment baseline for the Synara Worker KMS
signer:

- `values.production.yaml` targets `hashicorp/vault` chart `0.34.0`
- `helm-plugins/synara-vault-tls-readiness/plugin.yaml` and
  `helm-post-renderer.sh` provide the checked-in Helm 4 `postrenderer/v1`
  boundary that replaces the chart HTTPS kubelet readiness probe with an
  in-Pod Vault CLI probe using the mounted CA
- `values.production.yaml` pins the server image to `hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54`
- the chart renders three Raft peers with exact same-release peer traffic on
  8200/8201, client traffic from `synara-system` on 8200 only, a seal-aware
  liveness probe, a 30-second termination grace period, DNS egress only to the
  CoreDNS Pods in `kube-system`, Kubernetes Service egress only to
  `10.96.0.1/32:443`, direct Kubernetes API egress only to the dedicated Kind
  node subnet `192.168.155.0/24:6443`, and external SIEM egress only to
  `0.250.250.254/32:18443`
- the built-in Vault UI and chart UI service are disabled; operators use the
  Vault CLI through an authenticated administrative access path
- the server container exposes the mounted CA as
  `VAULT_CACERT=/vault/tls/ca.crt` so in-Pod operator CLI and unseal commands
  keep TLS verification enabled
- `values.production.yaml` forces the rendered readiness probe onto the HTTPS
  Vault health endpoint so the chart does not emit its built-in
  `vault status -tls-skip-verify` exec probe
- `manifests/` adds the `minAvailable: 2` PodDisruptionBudget the chart cannot
  express plus the non-secret `synara-vault-audit-observability` ConfigMap for
  audit shipping and rotation scripts
- `bootstrap/` contains a non-secret post-unseal bootstrap script plus signer,
  read-only production-auditor, and read-only snapshot-operator policies
- `operations-policy.json` captures the public KMS/tlog/admission boundary,
  Shamir custody, the isolated snapshot-restore drill, and audit/SIEM
  requirements

## Assumptions

- Namespace: `synara-kms`
- Helm release name: `synara-vault`
- TLS Secret: `synara-vault-server-tls`
  - keys: `tls.crt`, `tls.key`, `ca.crt`
- PVC-backed storage is available for both `/vault/data` and `/vault/audit`
- external SIEM Secret: `synara-vault-audit-siem`
  - keys: `VAULT_AUDIT_SIEM_ENDPOINT`, `VAULT_AUDIT_SIEM_CLIENT_CERT`,
    `VAULT_AUDIT_SIEM_CLIENT_KEY`, `VAULT_AUDIT_SIEM_CA_CERT`

The Service IP, Kubernetes API node subnet, SIEM host address, and Worker
Registry host checked into this Stage 3 baseline are concrete values for
`kind-synara-stage3-prod` on OrbStack. They are not portable defaults. Every
production cluster must render an reviewed per-cluster overlay and update both
the NetworkPolicy and `operations-policy.json` together before deployment; a
literal reuse of these addresses is a release failure.

If you run multiple Vault releases in the same namespace, tighten the selectors in
`values.production.yaml` and `manifests/vault-server-pdb.yaml` with
`app.kubernetes.io/instance`.

## Render locally

```bash
HELM_PLUGINS="$PWD/deploy/kubernetes/security/vault/helm-plugins" \
helm template synara-vault hashicorp/vault \
  --version 0.34.0 \
  --namespace synara-kms \
  -f deploy/kubernetes/security/vault/values.production.yaml \
  --post-renderer synara-vault-tls-readiness

kubectl kustomize deploy/kubernetes/security/vault/manifests
```

Helm 4 resolves `--post-renderer` by plugin name. Keep `HELM_PLUGINS` pinned to
the checked-in directory above; passing the shell script path directly is a
Helm 3-only invocation. Before an upgrade, inspect the final rendered
StatefulSet and require exactly one readiness exec command equal to
`["/bin/sh", "-ec", "vault status >/dev/null"]`, with no
`tls-skip-verify` and no readiness `httpGet` block.

## Bootstrap after init and unseal

Set `VAULT_ADDR`, `VAULT_CACERT`, and `VAULT_TOKEN` with a short-lived privileged
bootstrap token, then run:

```bash
deploy/kubernetes/security/vault/bootstrap/enable-transit-audit-approle.sh
```

The script:

- enables Transit at `transit/` if it is missing
- creates the non-exportable `synara-worker-release` signing key if it is missing
- converges Vault to exactly two PVC-backed file audit devices:
  `file -> /vault/audit/audit-primary.log` and
  `file-secondary -> /vault/audit/audit-secondary.log`
- enables AppRole if it is missing
- writes the signer policy and configures `synara-worker-release-signer` as a
  no-default-policy batch-token AppRole with a 2-hour TTL, 4-hour maximum TTL,
  and one-use 10-minute Secret IDs
- writes the read-only Raft/key/AppRole/audit/policy inspection policy and
  configures `synara-vault-production-auditor` as an independent
  no-default-policy batch-token AppRole with a 30-minute TTL and 1-hour maximum
  TTL
- writes the read-only snapshot/raft/key/AppRole/policy/audit-device inspection policy and
  configures `synara-vault-snapshot-operator` as an independent no-default-policy
  batch-token AppRole with a 30-minute TTL, 1-hour maximum TTL, and one-use
  10-minute Secret IDs

It does not print or store a Secret ID. It prints the follow-up `vault` commands you
must run to fetch `role_id` and mint a one-time `secret_id` through your secret manager.

The release gate consumes a signer token through `VAULT_TOKEN` and an independently
minted auditor token through `VAULT_OPERATOR_TOKEN`. They must differ. The signer
token is used only for `lookup-self`, Transit signing, and Cosign; the auditor token
is used only for read-only production introspection. Do not reuse the privileged
bootstrap token for either identity.

Use [docs/runbooks/vault-kms-operations.md](/Users/huang/devel/project/huang/business/synara/docs/runbooks/vault-kms-operations.md)
and `operations-policy.json` as the non-secret source of truth for Shamir custody,
the isolated snapshot-restore drill, and the audit rotation / external SIEM
boundary.

The StatefulSet also runs two non-root audit sidecars:

- `vault-audit-shipper` uses
  `timberio/vector:0.45.0-debian@sha256:987a15ebfb2eac3a4d5efb26252d140f799553feffb753dc215bdf738a7d4174`
  as Vault UID `100` / GID `1000` to read `/vault/audit/audit-primary.log`
  plus the newest plain-text primary archives, parse each JSON line into a
  structured event, persist its disk buffer on the audit PVC, and ship to the
  external collector over mTLS using the `synara-vault-audit-siem` Secret
  references above
- `vault-audit-rotation` uses
  `alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1`
  as Vault UID `100` / GID `1000` with `shareProcessNamespace: true` to rename
  `audit-primary.log` and `audit-secondary.log` in place at `100 MiB`, recreate
  mode-`0600` active files, send Vault `SIGHUP`, wait for `/proc/<pid>/fd`
  rollover away from the renamed archives, then gzip/prune older archives while
  retaining exactly `7` total archives per stream

Chart `0.34.0` cannot express a CA-verifying readiness exec through
`values.production.yaml` alone. The checked-in Helm 4 plugin rewrites only the
exact pinned HTTPS readiness block and fails closed on shape drift or any
`tls-skip-verify` occurrence. The resulting in-Pod `vault status` command uses
the chart-provided `VAULT_ADDR` and the pinned `VAULT_CACERT` mount, so the live
readiness boundary verifies the Vault serving certificate.
