#!/bin/sh
set -eu

umask 077
temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/synara-vault-post-render.XXXXXX")"
rendered_manifest="${temporary_directory}/rendered.yaml"
hardened_manifest="${temporary_directory}/hardened.yaml"

cleanup() {
  find "${temporary_directory}" -depth -delete 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

tee "${rendered_manifest}" >/dev/null

unsafe_probe='vault status -tls-skip-verify'
unsafe_occurrences="$(
  (grep -F -o "${unsafe_probe}" "${rendered_manifest}" || true) | wc -l | tr -d ' '
)"
if [ "${unsafe_occurrences}" -ne 0 ]; then
  echo "refusing a rendered Vault readiness probe that disables TLS verification" >&2
  exit 1
fi

# Helm 4 invokes a post-renderer once per rendered manifest file. Files that do
# not contain the Vault StatefulSet must remain byte-for-byte passthrough.
readiness_occurrences="$(
  (grep -E '^[[:space:]]+readinessProbe:$' "${rendered_manifest}" || true) | wc -l | tr -d ' '
)"
if [ "${readiness_occurrences}" -eq 0 ]; then
  cat "${rendered_manifest}"
  exit 0
fi
if [ "${readiness_occurrences}" -ne 1 ]; then
  echo "expected at most one Vault readiness probe per rendered manifest file" >&2
  exit 1
fi

# The chart turns readinessProbe.path into an HTTPS kubelet probe. Kubelet does
# not validate the serving certificate for HTTPS probes, so replace that exact
# chart-owned block with an in-Pod Vault CLI probe. The chart injects VAULT_ADDR
# and values.production.yaml pins VAULT_CACERT to the mounted CA.
awk '
  BEGIN { hardened = 0 }

  $0 ~ /^[[:space:]]+readinessProbe:$/ {
    if (hardened != 0) {
      print "multiple Vault readiness probes matched the hardening boundary" > "/dev/stderr"
      exit 41
    }

    indentation = $0
    sub(/readinessProbe:$/, "", indentation)
    print
    if ((getline http_get) <= 0 || http_get != indentation "  httpGet:") {
      print "the Vault readiness probe was not the expected HTTPS chart block" > "/dev/stderr"
      exit 42
    }
    if ((getline path) <= 0 || path != indentation "    path: \"/v1/sys/health?standbyok=true&perfstandbyok=true&sealedcode=204&uninitcode=204\"") {
      print "the Vault readiness health path drifted from the pinned baseline" > "/dev/stderr"
      exit 43
    }
    if ((getline port) <= 0 || port != indentation "    port: 8200") {
      print "the Vault readiness port drifted from the pinned baseline" > "/dev/stderr"
      exit 44
    }
    if ((getline scheme) <= 0 || scheme != indentation "    scheme: HTTPS") {
      print "the Vault readiness scheme drifted from the pinned baseline" > "/dev/stderr"
      exit 45
    }

    print indentation "  exec:"
    print indentation "    command:"
    print indentation "      - \"/bin/sh\""
    print indentation "      - \"-ec\""
    print indentation "      - \"vault status >/dev/null\""
    hardened = 1
    next
  }

  { print }

  END { if (hardened != 1) exit 46 }
' "${rendered_manifest}" > "${hardened_manifest}"

verified_occurrences="$(
  (grep -E '^[[:space:]]+- "vault status >/dev/null"$' "${hardened_manifest}" || true) | wc -l | tr -d ' '
)"
if [ "${verified_occurrences}" -ne 1 ]; then
  echo "the CA-verified Vault readiness probe was not rendered exactly once" >&2
  exit 1
fi

cat "${hardened_manifest}"
