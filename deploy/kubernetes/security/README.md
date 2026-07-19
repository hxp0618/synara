# Worker signing admission

These artifacts keep the production Worker image-signing controls separate from the
base Kubernetes install:

- `namespaced/` contains the ConfigMaps that hold the non-secret Cosign public key
  and the exact Synara Worker repository pattern.
- `cluster/` contains the Kyverno `ClusterPolicy` that requires valid signatures for
  matching Worker images and rewrites tags to immutable digests at admission time.
- `production/` is the only apply target. It renders the repository pattern from the
  namespaced ConfigMap into Kyverno's static `imageReferences` field; Kyverno does not
  permit variables in that field.
- `vault/` contains the pinned Vault HA chart values, the extra `minAvailable: 2`
  PodDisruptionBudget manifest, and a non-secret bootstrap script/policy for Transit,
  audit logging, and the signer AppRole.
- `registry/` contains a minimal production `distribution` blueprint with TLS, Basic
  auth, and delete disabled.

Before applying a cluster overlay:

1. Confirm the public key in
   `namespaced/synara-worker-cosign-public-key-configmap.yaml` matches the key
   exported from `hashivault://synara-worker-release`.
2. Set `namespaced/synara-worker-signing-settings-configmap.yaml` to the exact
   per-cluster production Worker repository pattern.
3. Install Kyverno and merge the private Registry CA into its existing trusted
   CA bundle.

Render and apply the ConfigMaps and policy atomically:

```bash
kubectl kustomize deploy/kubernetes/security/production
kubectl apply -k deploy/kubernetes/security/production
```

The base ClusterPolicy contains a fail-closed `registry.invalid` value and must
never be applied directly. The production kustomization replaces it from the
single repository ConfigMap source. The concrete repository host is a
per-cluster overlay, not a portable default. Do not put private keys, Vault
tokens, Registry credentials, or any other secret material in these manifests.
