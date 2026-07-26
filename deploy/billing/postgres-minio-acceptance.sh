#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
control_plane_dir="$repo_root/services/control-plane"
fixture_file="$control_plane_dir/internal/billing/testdata/aws_cur_fixture.csv"

postgres_image="${SYNARA_BILLING_ACCEPTANCE_POSTGRES_IMAGE:-postgres:17-alpine}"
minio_image="${SYNARA_BILLING_ACCEPTANCE_MINIO_IMAGE:-minio/minio:RELEASE.2025-04-22T22-12-26Z}"
mc_image="${SYNARA_BILLING_ACCEPTANCE_MC_IMAGE:-minio/mc:RELEASE.2025-04-16T18-13-26Z}"
test_runtime_image="${SYNARA_BILLING_ACCEPTANCE_TEST_RUNTIME_IMAGE:-golang:1.26-bookworm@sha256:e60d708a92ad26a6d61901334510d3debd23ddcba125663ecd6008d42e8ec669}"
evidence_file="${SYNARA_BILLING_ACCEPTANCE_EVIDENCE_FILE:-}"
acceptance_generation="${SYNARA_BILLING_ACCEPTANCE_GENERATION:-final13}"
runtime_test_name='TestBillingRuntimePostgresVersionedS3ImportEstimateReconcileReplay'
cur2_manifest_test_name='TestBillingCUR2ManifestVersionedS3Acceptance'
cur2_child_only_subtest="${cur2_manifest_test_name}/child_only_rejected"
cur2_family_orphan_subtest="${cur2_manifest_test_name}/net_parent_gross_child_rejected"
cur2_cross_chunk_orphan_subtest="${cur2_manifest_test_name}/cross_chunk_missing_parent_rejected"
concurrent_import_test_name='TestPostgresConcurrentInitialInvoiceImportsSerializeByIdentity'
concurrent_same_subtest="${concurrent_import_test_name}/same_checksum_returns_the_committed_identity_to_the_scheduler_path"
concurrent_conflict_subtest="${concurrent_import_test_name}/different_checksum_remains_a_stable_conflict"

run_started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
run_id="$(date -u +%Y%m%d%H%M%S)-$$-${RANDOM}"
name_suffix="$(printf '%s' "$run_id" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
name_suffix="${name_suffix:0:30}"
network_name="synara-billing-$name_suffix"
postgres_container="synara-billing-pg-$name_suffix"
minio_container="synara-billing-s3-$name_suffix"
bucket_name="synara-billing-$name_suffix"
object_key='runtime/aws-cur.csv'

temp_dir=''
detail_file=''
poison_file=''
evidence_reserved=0
network_created=0
postgres_created=0
minio_created=0
network_attempted=0
postgres_attempted=0
minio_attempted=0
cleanup_failed=0
cleanup_status='not-run'
runtime_removed=false
exact_object_version_pinned=false
finalized=0
phase='preflight'
result_phase='preflight'
failure_reason=''
test_status='not-run'
test_exit_code=0
detail_produced=false
detail_valid=false
detail_bounded=false
detail_secret_free=false
detail_assertions_valid=false
detail_bytes=0
detail_digest=''
integration_assertions='{}'
source_sha='unknown'
worktree_dirty=false
postgres_image_id='unknown'
minio_image_id='unknown'
mc_image_id='unknown'
test_runtime_image_id='unknown'
test_runtime_arch='unknown'
postgres_version='unknown'
host_go_version='unknown'
acceptance_script_digest=''
runtime_test_source_digest=''
concurrent_import_test_source_digest=''
cur2_manifest_test_source_digest=''
billing_service_source_digest=''
blob_parsers_source_digest=''
blob_source_source_digest=''
cloud_sources_source_digest=''
test_binary_digest=''
test_binary_unchanged=false
billing_source_files_unchanged=false
required_test_cases_passed=false
object_version=''
replacement_object_version=''
network_id=''
postgres_container_id=''
minio_container_id=''

postgres_password=''
minio_access_key=''
minio_secret_key=''
postgres_host_port=''
minio_host_port=''
database_url=''
minio_endpoint=''
postgres_env_file=''
minio_env_file=''
mc_env_file=''
test_env_file=''
test_binary=''
runtime_test_events=''
concurrent_import_test_events=''
cur2_manifest_test_events=''
transient_container_ids=()
transient_container_names=()
transient_counter=0
current_version_result=''

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

fail() {
  failure_reason="$1"
  printf '%s\n' "$failure_reason" >&2
  exit 1
}

run_bounded() {
  local timeout_seconds="$1"
  shift
  python3 - "$timeout_seconds" "$@" <<'PY'
import subprocess
import sys

timeout = float(sys.argv[1])
try:
    completed = subprocess.run(sys.argv[2:], capture_output=True, timeout=timeout, check=False)
except subprocess.TimeoutExpired as error:
    if error.stdout:
        sys.stdout.buffer.write(error.stdout)
    if error.stderr:
        sys.stderr.buffer.write(error.stderr)
    raise SystemExit(124)
sys.stdout.buffer.write(completed.stdout)
sys.stderr.buffer.write(completed.stderr)
raise SystemExit(completed.returncode)
PY
}

remove_owned_container() {
  local container_id="$1" expected_name="$2" output current_id current_name current_owner
  if [[ -z "$container_id" ]]; then
    printf 'Cannot prove cleanup for attempted container without an exact ID: %s\n' "$expected_name" >&2
    return 1
  fi
  if output="$(run_bounded 20 docker container inspect "$container_id" 2>&1)"; then
    current_id="$(jq -er '.[0].Id' <<<"$output")" || return 1
    current_name="$(jq -er '.[0].Name' <<<"$output")" || return 1
    current_owner="$(jq -er '.[0].Config.Labels["synara.io/acceptance-run-id"] // empty' <<<"$output")" || return 1
  else
    if [[ "$output" == *'No such container'* || "$output" == *'No such object'* ]]; then
      return 0
    fi
    printf 'Container cleanup presence check failed for %s\n' "$expected_name" >&2
    return 1
  fi
  if [[ "$current_id" != "$container_id" || "$current_name" != "/$expected_name" || "$current_owner" != "$run_id" ]]; then
    printf 'Refusing to remove container with changed exact identity: %s\n' "$expected_name" >&2
    return 1
  fi
  if ! run_bounded 30 docker container rm -f -v "$container_id" >/dev/null; then
    printf 'Failed to remove exact acceptance container: %s\n' "$expected_name" >&2
    return 1
  fi
  if output="$(run_bounded 20 docker container inspect "$container_id" 2>&1)"; then
    printf 'Exact acceptance container still exists after cleanup: %s\n' "$expected_name" >&2
    return 1
  fi
  if [[ "$output" != *'No such container'* && "$output" != *'No such object'* ]]; then
    printf 'Could not confirm exact acceptance container removal: %s\n' "$expected_name" >&2
    return 1
  fi
}

remove_owned_network() {
  local output current_id current_name current_owner
  if [[ -z "$network_id" ]]; then
    printf 'Cannot prove cleanup for attempted network without an exact ID: %s\n' "$network_name" >&2
    return 1
  fi
  if output="$(run_bounded 20 docker network inspect "$network_id" 2>&1)"; then
    current_id="$(jq -er '.[0].Id' <<<"$output")" || return 1
    current_name="$(jq -er '.[0].Name' <<<"$output")" || return 1
    current_owner="$(jq -er '.[0].Labels["synara.io/acceptance-run-id"] // empty' <<<"$output")" || return 1
  else
    if [[ "$output" == *'No such network'* || "$output" == *'not found'* ]]; then
      return 0
    fi
    printf 'Network cleanup presence check failed for %s\n' "$network_name" >&2
    return 1
  fi
  if [[ "$current_id" != "$network_id" || "$current_name" != "$network_name" || "$current_owner" != "$run_id" ]]; then
    printf 'Refusing to remove network with changed exact identity: %s\n' "$network_name" >&2
    return 1
  fi
  if ! run_bounded 30 docker network rm "$network_id" >/dev/null; then
    printf 'Failed to remove exact acceptance network: %s\n' "$network_name" >&2
    return 1
  fi
  if output="$(run_bounded 20 docker network inspect "$network_id" 2>&1)"; then
    printf 'Exact acceptance network still exists after cleanup: %s\n' "$network_name" >&2
    return 1
  fi
  if [[ "$output" != *'No such network'* && "$output" != *'not found'* ]]; then
    printf 'Could not confirm exact acceptance network removal: %s\n' "$network_name" >&2
    return 1
  fi
}

wait_for_postgres() {
  local attempt
  for attempt in $(seq 1 90); do
    if docker exec "$postgres_container_id" \
      psql -U synara -d synara -Atqc 'SELECT 1' 2>/dev/null | grep -Fxq '1'; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_minio() {
  local attempt
  for attempt in $(seq 1 90); do
    if docker exec "$minio_container_id" \
      curl --fail --silent --show-error --max-time 2 http://127.0.0.1:9000/minio/health/ready >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

run_mc() {
  local purpose_name container_id command_status=0 cleanup_status=0
  transient_counter=$((transient_counter + 1))
  purpose_name="synara-billing-mc-$name_suffix-$transient_counter"
  container_id="$(docker create \
    --name "$purpose_name" \
    --network "$network_id" \
    --label "synara.io/acceptance-run-id=$run_id" \
    --env-file "$mc_env_file" \
    -v "$control_plane_dir/internal/billing/testdata:/fixtures:ro" \
    -v "$temp_dir:/acceptance:ro" \
    "$mc_image_id" "$@")" || return 1
  if [[ ! "$container_id" =~ ^[0-9a-f]{64}$ ]]; then
    printf 'Docker did not return an exact MC container ID\n' >&2
    return 1
  fi
  transient_container_ids+=("$container_id")
  transient_container_names+=("$purpose_name")
  run_bounded 60 docker start -a "$container_id" || command_status="$?"
  remove_owned_container "$container_id" "$purpose_name" || cleanup_status="$?"
  if [[ "$cleanup_status" -ne 0 ]]; then
    return "$cleanup_status"
  fi
  return "$command_status"
}

current_object_version() {
  local payload version output_file
  output_file="$temp_dir/mc-stat-$((transient_counter + 1)).json"
  run_mc stat --json "acceptance/$bucket_name/$object_key" >"$output_file" || return 1
  payload="$(<"$output_file")"
  version="$(jq -er '.versionID // .versionId' <<<"$payload")" || return 1
  if [[ ${#version} -gt 255 || ! "$version" =~ ^[A-Za-z0-9._~+=-]+$ ]]; then
    return 1
  fi
  current_version_result="$version"
}

finalize_detail() {
  local detail_content
  local allowed_assertion_keys='[
    "estimateTariffSegmentsPersisted",
    "exactObjectVersionRead",
    "invoiceImported",
    "postgresMigrationsApplied",
    "reconciliationPersisted",
    "restartReplayIdempotent",
    "scheduledAuditIdempotent"
  ]'
  [[ -f "$detail_file" ]] || return 0
  detail_produced=true
  detail_bytes="$(wc -c <"$detail_file" | tr -d '[:space:]')"
  detail_digest="$(shasum -a 256 "$detail_file" | awk '{print $1}')"
  if jq -e . "$detail_file" >/dev/null 2>&1; then detail_valid=true; fi
  if (( detail_bytes <= 65536 )); then detail_bounded=true; fi
  detail_content="$(<"$detail_file")"
  if [[ "$detail_content" != *"$postgres_password"* && \
    "$detail_content" != *"$minio_access_key"* && \
    "$detail_content" != *"$minio_secret_key"* && \
    "$detail_content" != *"$database_url"* ]]; then
    detail_secret_free=true
  fi
  if [[ "$detail_valid" == true && "$detail_bounded" == true ]]; then
    if jq -e --argjson allowed "$allowed_assertion_keys" '
      (.assertions | type) == "object" and
      ((.assertions | keys | sort) == ($allowed | sort)) and
      all(.assertions[]; type == "boolean" and . == true)
    ' "$detail_file" >/dev/null; then
      integration_assertions="$(jq -c '{
        estimateTariffSegmentsPersisted: .assertions.estimateTariffSegmentsPersisted,
        exactObjectVersionRead: .assertions.exactObjectVersionRead,
        invoiceImported: .assertions.invoiceImported,
        postgresMigrationsApplied: .assertions.postgresMigrationsApplied,
        reconciliationPersisted: .assertions.reconciliationPersisted,
        restartReplayIdempotent: .assertions.restartReplayIdempotent,
        scheduledAuditIdempotent: .assertions.scheduledAuditIdempotent
      }' "$detail_file")"
      detail_assertions_valid=true
    fi
  fi
}

validate_required_test_events() {
  [[ -f "$runtime_test_events" && -f "$concurrent_import_test_events" && -f "$cur2_manifest_test_events" ]] || return 1
  jq -s -e \
    --arg runtime "$runtime_test_name" \
    --arg cur2 "$cur2_manifest_test_name" \
	--arg cur2ChildOnly "$cur2_child_only_subtest" \
	--arg cur2FamilyOrphan "$cur2_family_orphan_subtest" \
	--arg cur2CrossChunkOrphan "$cur2_cross_chunk_orphan_subtest" \
    --arg concurrent "$concurrent_import_test_name" \
    --arg concurrentSame "$concurrent_same_subtest" \
    --arg concurrentConflict "$concurrent_conflict_subtest" '
      def action_count($test; $action):
        [.[] | select(.Test == $test and .Action == $action)] | length;
      def rejected_outcomes($root):
        [
          .[]
          | select(
              (((.Test // "") == $root) or ((.Test // "") | startswith($root + "/")))
              and (.Action == "skip" or .Action == "fail")
            )
        ] | length;
      action_count($runtime; "run") == 1 and
      action_count($runtime; "pass") == 1 and
      rejected_outcomes($runtime) == 0 and
      action_count($cur2; "run") == 1 and
      action_count($cur2; "pass") == 1 and
	  action_count($cur2ChildOnly; "run") == 1 and
	  action_count($cur2ChildOnly; "pass") == 1 and
	  action_count($cur2FamilyOrphan; "run") == 1 and
	  action_count($cur2FamilyOrphan; "pass") == 1 and
	  action_count($cur2CrossChunkOrphan; "run") == 1 and
	  action_count($cur2CrossChunkOrphan; "pass") == 1 and
      rejected_outcomes($cur2) == 0 and
      action_count($concurrent; "run") == 1 and
      action_count($concurrent; "pass") == 1 and
      action_count($concurrentSame; "pass") == 1 and
      action_count($concurrentConflict; "pass") == 1 and
      rejected_outcomes($concurrent) == 0
    ' "$runtime_test_events" "$concurrent_import_test_events" "$cur2_manifest_test_events" >/dev/null
}

write_evidence() {
  local status="$1" exit_code="$2" finished_at evidence_tmp
  [[ "$evidence_reserved" == 1 ]] || return 0
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  evidence_tmp="$(mktemp "$(dirname "$evidence_file")/.synara-billing-evidence.XXXXXX")"
  if ! jq -n \
	--arg schemaVersion 'synara.billing-postgres-minio-acceptance.v3' \
	--arg acceptanceGeneration "$acceptance_generation" \
    --arg runID "$run_id" --arg startedAt "$run_started_at" --arg finishedAt "$finished_at" \
    --arg status "$status" --arg phase "$result_phase" --arg failure "$failure_reason" \
    --arg runtimeTestName "$runtime_test_name" --arg cur2ManifestTestName "$cur2_manifest_test_name" \
	--arg cur2ChildOnlySubtest "$cur2_child_only_subtest" \
	--arg cur2FamilyOrphanSubtest "$cur2_family_orphan_subtest" \
	--arg cur2CrossChunkOrphanSubtest "$cur2_cross_chunk_orphan_subtest" \
    --arg concurrentImportTestName "$concurrent_import_test_name" \
    --arg testStatus "$test_status" --argjson testExitCode "$test_exit_code" \
    --argjson detailProduced "$detail_produced" --argjson detailValid "$detail_valid" \
    --argjson detailBounded "$detail_bounded" --argjson detailSecretFree "$detail_secret_free" \
    --argjson detailAssertionsValid "$detail_assertions_valid" --argjson detailBytes "$detail_bytes" \
    --arg detailDigest "$detail_digest" --argjson assertions "$integration_assertions" \
    --arg sourceSHA "$source_sha" --argjson worktreeDirty "$worktree_dirty" \
    --arg hostGoVersion "$host_go_version" --arg acceptanceScriptDigest "$acceptance_script_digest" \
    --arg runtimeTestSourceDigest "$runtime_test_source_digest" \
    --arg concurrentImportTestSourceDigest "$concurrent_import_test_source_digest" \
	--arg cur2ManifestTestSourceDigest "$cur2_manifest_test_source_digest" \
    --arg billingServiceSourceDigest "$billing_service_source_digest" --arg testBinaryDigest "$test_binary_digest" \
	--arg blobParsersSourceDigest "$blob_parsers_source_digest" \
	--arg blobSourceSourceDigest "$blob_source_source_digest" \
	--arg cloudSourcesSourceDigest "$cloud_sources_source_digest" \
    --argjson testBinaryUnchanged "$test_binary_unchanged" \
	--argjson billingSourceFilesUnchanged "$billing_source_files_unchanged" \
    --argjson requiredTestCasesPassed "$required_test_cases_passed" \
    --arg postgresImage "$postgres_image" --arg postgresImageID "$postgres_image_id" \
    --arg postgresVersion "$postgres_version" --arg minioImage "$minio_image" \
    --arg minioImageID "$minio_image_id" --arg mcImage "$mc_image" --arg mcImageID "$mc_image_id" \
    --arg testRuntimeImage "$test_runtime_image" --arg testRuntimeImageID "$test_runtime_image_id" \
    --arg testRuntimeArch "$test_runtime_arch" \
    --arg cleanupStatus "$cleanup_status" --argjson exitCode "$exit_code" \
    --argjson exactObjectVersionPinned "$exact_object_version_pinned" \
    --argjson runtimeRemoved "$runtime_removed" '
      {
		schemaVersion: $schemaVersion, acceptanceGeneration: $acceptanceGeneration,
		runID: $runID, startedAt: $startedAt, finishedAt: $finishedAt,
        status: $status, exitCode: $exitCode, phase: $phase,
        failure: (if $failure == "" then null else $failure[0:256] end),
        test: {
          cases: [$runtimeTestName, $cur2ManifestTestName, $concurrentImportTestName],
		  requiredSubtests: [$cur2ChildOnlySubtest, $cur2FamilyOrphanSubtest, $cur2CrossChunkOrphanSubtest],
		  invocations: [
			{root: $concurrentImportTestName, exactRunPattern: ("^" + $concurrentImportTestName + "$")},
			{root: $runtimeTestName, exactRunPattern: ("^" + $runtimeTestName + "$")},
			{root: $cur2ManifestTestName, exactRunPattern: ("^" + $cur2ManifestTestName + "$")}
		  ],
          status: $testStatus, exitCode: $testExitCode
        },
        checks: {
          detailProduced: $detailProduced, detailValidJSON: $detailValid,
          detailWithin65536Bytes: $detailBounded, detailContainsNoCredentials: $detailSecretFree,
          detailAssertionsAllowlistedBooleans: $detailAssertionsValid,
          testBinaryUnchangedDuringRun: $testBinaryUnchanged,
		  billingSourceFilesUnchangedDuringRun: $billingSourceFilesUnchanged,
          requiredTestCasesPassedWithoutSkip: $requiredTestCasesPassed,
		  billingSourceFilesBoundToEvidence: (
			($blobParsersSourceDigest | test("^[0-9a-f]{64}$")) and
			($blobSourceSourceDigest | test("^[0-9a-f]{64}$")) and
			($cloudSourcesSourceDigest | test("^[0-9a-f]{64}$"))
		  ),
          integrationAssertions: $assertions
        },
        detail: {bytes: $detailBytes, sha256: (if $detailDigest == "" then null else $detailDigest end)},
        source: {
          gitSHA: $sourceSHA, worktreeDirty: $worktreeDirty, hostGoVersion: $hostGoVersion,
          acceptanceScriptSHA256: (if $acceptanceScriptDigest == "" then null else $acceptanceScriptDigest end),
          runtimeTestSourceSHA256: (if $runtimeTestSourceDigest == "" then null else $runtimeTestSourceDigest end),
          concurrentImportTestSourceSHA256: (if $concurrentImportTestSourceDigest == "" then null else $concurrentImportTestSourceDigest end),
		  cur2ManifestTestSourceSHA256: (if $cur2ManifestTestSourceDigest == "" then null else $cur2ManifestTestSourceDigest end),
          billingServiceSourceSHA256: (if $billingServiceSourceDigest == "" then null else $billingServiceSourceDigest end),
		  blobParsersSourceSHA256: (if $blobParsersSourceDigest == "" then null else $blobParsersSourceDigest end),
		  blobSourceSHA256: (if $blobSourceSourceDigest == "" then null else $blobSourceSourceDigest end),
		  cloudSourcesSourceSHA256: (if $cloudSourcesSourceDigest == "" then null else $cloudSourcesSourceDigest end),
          testBinarySHA256: (if $testBinaryDigest == "" then null else $testBinaryDigest end)
        },
        runtime: {
          metadataStore: "disposable-postgresql",
          objectStore: "disposable-versioned-minio-s3-compatible",
          exactObjectVersionPinned: $exactObjectVersionPinned,
		  cur2NativeManifestVersionedS3Verified: $requiredTestCasesPassed,
          cloudWorkloadIdentityVerified: false
        },
        images: {
          postgres: {reference: $postgresImage, id: $postgresImageID, version: $postgresVersion},
          minio: {reference: $minioImage, id: $minioImageID},
          mc: {reference: $mcImage, id: $mcImageID},
          testRuntime: {reference: $testRuntimeImage, id: $testRuntimeImageID, architecture: $testRuntimeArch}
        },
        cleanup: {status: $cleanupStatus},
        safety: {
          disposableRuntimeRemoved: $runtimeRemoved,
          credentialsPersistedAfterCleanup: (if $runtimeRemoved then false else null end),
          objectContentsPersistedAfterCleanup: (if $runtimeRemoved then false else null end)
        }
      }' >"$evidence_tmp"; then
    rm -f "$evidence_tmp"
    return 1
  fi
  if ! mv -f "$evidence_tmp" "$evidence_file"; then
    rm -f "$evidence_tmp"
    return 1
  fi
}

cleanup_all() {
  cleanup_status='passed'
  phase='cleanup'
  local index
  for ((index = ${#transient_container_ids[@]} - 1; index >= 0; index--)); do
    remove_owned_container "${transient_container_ids[$index]}" "${transient_container_names[$index]}" || cleanup_failed=1
  done
  if [[ "$minio_attempted" == 1 ]]; then
    remove_owned_container "$minio_container_id" "$minio_container" || cleanup_failed=1
  fi
  if [[ "$postgres_attempted" == 1 ]]; then
    remove_owned_container "$postgres_container_id" "$postgres_container" || cleanup_failed=1
  fi
  if [[ "$network_attempted" == 1 ]]; then remove_owned_network || cleanup_failed=1; fi
  postgres_password=''
  minio_access_key=''
  minio_secret_key=''
  database_url=''
  if [[ -n "$temp_dir" && -d "$temp_dir" ]]; then
    if ! rm -rf "$temp_dir"; then cleanup_failed=1; fi
  fi
  if [[ "$cleanup_failed" == 1 ]]; then
    cleanup_status='failed'
  else
    runtime_removed=true
  fi
}

on_exit() {
  local exit_code="$?" final_status='failed'
  trap - EXIT ERR INT TERM
  if [[ "$finalized" == 1 ]]; then exit "$exit_code"; fi
  if [[ -z "$failure_reason" && "$exit_code" -ne 0 ]]; then
    failure_reason="acceptance failed during $phase"
  fi
  result_phase="$phase"
  cleanup_all
  if [[ "$exit_code" -eq 0 && "$test_status" == passed && "$cleanup_failed" == 0 ]]; then
    final_status='passed'
  elif [[ "$exit_code" -eq 0 ]]; then
    exit_code=1
  fi
  phase='complete'
  if ! write_evidence "$final_status" "$exit_code"; then
    printf 'Failed to write bounded evidence to %s\n' "$evidence_file" >&2
    exit_code=1
  fi
  finalized=1
  exit "$exit_code"
}
trap on_exit EXIT
trap 'failure_reason="acceptance interrupted"; exit 130' INT TERM

require_command docker
require_command git
require_command go
require_command jq
require_command mktemp
require_command openssl
require_command python3
require_command seq
require_command shasum

[[ -n "$evidence_file" ]] || fail 'SYNARA_BILLING_ACCEPTANCE_EVIDENCE_FILE must be an explicit JSON output path'
if [[ ! "$acceptance_generation" =~ ^final([1-9][0-9]*)$ || "${BASH_REMATCH[1]}" -lt 13 ]]; then
  fail 'SYNARA_BILLING_ACCEPTANCE_GENERATION must be final13 or higher'
fi
[[ ! -e "$evidence_file" ]] || fail "evidence path already exists: $evidence_file"
[[ -d "$(dirname "$evidence_file")" ]] || fail "evidence parent directory does not exist: $(dirname "$evidence_file")"
[[ -f "$fixture_file" ]] || fail "billing runtime acceptance fixture is missing: $fixture_file"
for name in "$network_name" "$postgres_container" "$minio_container"; do
  if docker container inspect "$name" >/dev/null 2>&1 || docker network inspect "$name" >/dev/null 2>&1; then
    fail "acceptance Docker resource already exists: $name"
  fi
done

source_sha="$(git -C "$repo_root" rev-parse HEAD)"
if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ && ! "$source_sha" =~ ^[0-9a-f]{64}$ ]]; then
  fail 'repository HEAD did not resolve to a full hexadecimal object ID'
fi
if [[ -n "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]]; then worktree_dirty=true; fi
host_go_version="$(go version)"
acceptance_script_digest="$(shasum -a 256 "$script_dir/postgres-minio-acceptance.sh" | awk '{print $1}')"
runtime_test_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/runtime_postgres_integration_test.go" | awk '{print $1}')"
concurrent_import_test_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/invoice_import_postgres_integration_test.go" | awk '{print $1}')"
cur2_manifest_test_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/cur2_manifest_s3_integration_test.go" | awk '{print $1}')"
billing_service_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/service.go" | awk '{print $1}')"
blob_parsers_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/blob_parsers.go" | awk '{print $1}')"
blob_source_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/blob_source.go" | awk '{print $1}')"
cloud_sources_source_digest="$(shasum -a 256 "$control_plane_dir/internal/billing/cloud_sources.go" | awk '{print $1}')"

phase='resolve-images'
for image in "$postgres_image" "$minio_image" "$mc_image" "$test_runtime_image"; do
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker pull "$image" >/dev/null
  fi
done
postgres_image_id="$(docker image inspect --format '{{.Id}}' "$postgres_image")"
minio_image_id="$(docker image inspect --format '{{.Id}}' "$minio_image")"
mc_image_id="$(docker image inspect --format '{{.Id}}' "$mc_image")"
test_runtime_image_id="$(docker image inspect --format '{{.Id}}' "$test_runtime_image")"
test_runtime_arch="$(docker image inspect --format '{{.Architecture}}' "$test_runtime_image")"
case "$test_runtime_arch" in
  amd64|arm64) ;;
  *) fail "unsupported Billing acceptance test runtime architecture: $test_runtime_arch" ;;
esac

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/synara-billing-acceptance.XXXXXX")"
detail_file="$temp_dir/test-detail.json"
poison_file="$temp_dir/latest-poison.csv"
postgres_env_file="$temp_dir/postgres.env"
minio_env_file="$temp_dir/minio.env"
mc_env_file="$temp_dir/mc.env"
test_env_file="$temp_dir/test.env"
test_binary="$temp_dir/billing.test"
runtime_test_events="$temp_dir/runtime-test-events.jsonl"
concurrent_import_test_events="$temp_dir/concurrent-import-test-events.jsonl"
cur2_manifest_test_events="$temp_dir/cur2-manifest-test-events.jsonl"
printf '%s\n' 'this-latest-version-must-not-be-read' >"$poison_file"
if ! (set -o noclobber; : >"$evidence_file") 2>/dev/null; then
  fail "could not exclusively reserve evidence path: $evidence_file"
fi
evidence_reserved=1

postgres_password="$(openssl rand -hex 24)"
minio_access_key="synara$name_suffix"
minio_secret_key="$(openssl rand -hex 24)"
printf 'POSTGRES_USER=synara\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=synara\n' \
  "$postgres_password" >"$postgres_env_file"
printf 'MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\n' \
  "$minio_access_key" "$minio_secret_key" >"$minio_env_file"
printf 'MC_HOST_acceptance=http://%s:%s@%s:9000\n' \
  "$minio_access_key" "$minio_secret_key" "$minio_container" >"$mc_env_file"
chmod 600 "$postgres_env_file" "$minio_env_file" "$mc_env_file"

phase='build-linux-test-binary'
(
  cd "$control_plane_dir"
  env CGO_ENABLED=0 GOOS=linux GOARCH="$test_runtime_arch" \
    go test -c -o "$test_binary" ./internal/billing
)
chmod 700 "$test_binary"
test_binary_digest="$(shasum -a 256 "$test_binary" | awk '{print $1}')"

phase='create-network'
network_attempted=1
network_id="$(docker network create --internal --label "synara.io/acceptance-run-id=$run_id" "$network_name")"
[[ "$network_id" =~ ^[0-9a-f]{64}$ ]] || fail 'Docker did not return the exact acceptance network ID'
network_created=1

phase='start-postgres'
postgres_attempted=1
postgres_created=1
postgres_container_id="$(docker create \
  --name "$postgres_container" \
  --network "$network_id" \
  --label "synara.io/acceptance-run-id=$run_id" \
  --env-file "$postgres_env_file" \
  --tmpfs /var/lib/postgresql/data:rw,noexec,nosuid,size=512m \
  "$postgres_image_id")"
[[ "$postgres_container_id" =~ ^[0-9a-f]{64}$ ]] || fail 'Docker did not return the exact PostgreSQL container ID'
[[ "$(docker container inspect --format '{{.Image}}' "$postgres_container_id")" == "$postgres_image_id" ]] || \
  fail 'PostgreSQL container did not freeze the resolved image ID'
docker container inspect "$postgres_container_id" | jq -e '
  .[0].HostConfig.Tmpfs["/var/lib/postgresql/data"] != null and
  ([.[0].Mounts[]? | select(.Destination == "/var/lib/postgresql/data" and .Type == "volume")] | length) == 0
' >/dev/null || fail 'PostgreSQL acceptance data is not bound to container-lifetime tmpfs'
docker start "$postgres_container_id" >/dev/null

phase='start-minio'
minio_attempted=1
minio_created=1
minio_container_id="$(docker create \
  --name "$minio_container" \
  --network "$network_id" \
  --label "synara.io/acceptance-run-id=$run_id" \
  --env-file "$minio_env_file" \
  --tmpfs /data:rw,noexec,nosuid,size=512m \
  "$minio_image_id" server /data)"
[[ "$minio_container_id" =~ ^[0-9a-f]{64}$ ]] || fail 'Docker did not return the exact MinIO container ID'
[[ "$(docker container inspect --format '{{.Image}}' "$minio_container_id")" == "$minio_image_id" ]] || \
  fail 'MinIO container did not freeze the resolved image ID'
docker container inspect "$minio_container_id" | jq -e '
  .[0].HostConfig.Tmpfs["/data"] != null and
  ([.[0].Mounts[]? | select(.Destination == "/data" and .Type == "volume")] | length) == 0
' >/dev/null || fail 'MinIO acceptance data is not bound to container-lifetime tmpfs'
docker start "$minio_container_id" >/dev/null

database_url="postgres://synara:$postgres_password@$postgres_container:5432/synara?sslmode=disable"
minio_endpoint="http://$minio_container:9000"

phase='wait-runtime'
wait_for_postgres || fail 'disposable PostgreSQL did not become ready'
wait_for_minio || fail 'disposable MinIO did not become ready'

postgres_version="$(docker exec "$postgres_container_id" \
  psql -U synara -d synara -Atqc 'SHOW server_version')"

phase='seed-versioned-object'
run_mc mb "acceptance/$bucket_name" >/dev/null
run_mc version enable "acceptance/$bucket_name" >/dev/null
run_mc cp --json /fixtures/aws_cur_fixture.csv "acceptance/$bucket_name/$object_key" >/dev/null
current_object_version || fail 'MinIO stat did not return the exact fixture VersionId'
object_version="$current_version_result"
run_mc cp --json /acceptance/latest-poison.csv "acceptance/$bucket_name/$object_key" >/dev/null
current_object_version || fail 'MinIO stat did not return the replacement VersionId'
replacement_object_version="$current_version_result"
[[ "$object_version" != "$replacement_object_version" ]] || fail 'versioned MinIO object replacement reused the fixture VersionId'
exact_object_version_pinned=true

printf '%s\n' \
  "SYNARA_TEST_DATABASE_URL=$database_url" \
  "SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_ENDPOINT=$minio_endpoint" \
  "SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_BUCKET=$bucket_name" \
  "SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_KEY=$object_key" \
  "SYNARA_BILLING_RUNTIME_ACCEPTANCE_S3_OBJECT_VERSION=$object_version" \
  'SYNARA_BILLING_RUNTIME_ACCEPTANCE_EVIDENCE_DETAIL_FILE=/acceptance/test-detail.json' \
  "AWS_ACCESS_KEY_ID=$minio_access_key" \
  "AWS_SECRET_ACCESS_KEY=$minio_secret_key" \
  'AWS_SESSION_TOKEN=' \
  'AWS_EC2_METADATA_DISABLED=true' \
  'AWS_REGION=us-east-1' >"$test_env_file"
chmod 600 "$test_env_file"

phase='run-go-integration-test'
test_container_name="synara-billing-test-$name_suffix"
test_container_id="$(docker create \
  --name "$test_container_name" \
  --network "$network_id" \
  --label "synara.io/acceptance-run-id=$run_id" \
  --env-file "$test_env_file" \
  -v "$temp_dir:/acceptance" \
  "$test_runtime_image_id" \
  /bin/sh -eu -c '
    go tool test2json -p synara-billing-acceptance -t \
      /acceptance/billing.test -test.v=test2json -test.run "^${1}$" -test.count=1 \
      > /acceptance/concurrent-import-test-events.jsonl
    go tool test2json -p synara-billing-acceptance -t \
      /acceptance/billing.test -test.v=test2json -test.run "^${2}$" -test.count=1 \
      > /acceptance/runtime-test-events.jsonl
    go tool test2json -p synara-billing-acceptance -t \
      /acceptance/billing.test -test.v=test2json -test.run "^${3}$" -test.count=1 \
      > /acceptance/cur2-manifest-test-events.jsonl
  ' sh "$concurrent_import_test_name" "$runtime_test_name" "$cur2_manifest_test_name")"
[[ "$test_container_id" =~ ^[0-9a-f]{64}$ ]] || fail 'Docker did not return the exact Billing test container ID'
transient_container_ids+=("$test_container_id")
transient_container_names+=("$test_container_name")
if run_bounded 180 docker start -a "$test_container_id"; then
  test_status='passed'
  test_exit_code=0
else
  test_exit_code="$?"
  test_status='failed'
  failure_reason="Go integration test failed with exit code $test_exit_code"
fi
if ! remove_owned_container "$test_container_id" "$test_container_name"; then
  failure_reason='Billing test container cleanup failed'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi
if current_test_binary_digest="$(shasum -a 256 "$test_binary" | awk '{print $1}')" && \
  [[ "$current_test_binary_digest" == "$test_binary_digest" ]]; then
  test_binary_unchanged=true
else
  failure_reason='Billing test binary changed or became unreadable during the acceptance run'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi
if [[ "$(shasum -a 256 "$control_plane_dir/internal/billing/blob_parsers.go" | awk '{print $1}')" == "$blob_parsers_source_digest" && \
  "$(shasum -a 256 "$control_plane_dir/internal/billing/blob_source.go" | awk '{print $1}')" == "$blob_source_source_digest" && \
  "$(shasum -a 256 "$control_plane_dir/internal/billing/cloud_sources.go" | awk '{print $1}')" == "$cloud_sources_source_digest" ]]; then
  billing_source_files_unchanged=true
else
  failure_reason='Billing CUR2 source files changed during the acceptance run'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi
if validate_required_test_events; then
  required_test_cases_passed=true
else
	for event_file in "$runtime_test_events" "$cur2_manifest_test_events" "$concurrent_import_test_events"; do
		if [[ -f "$event_file" ]]; then
			jq -r 'select(.Action == "output" or .Action == "fail") | ((.Test // "billing") + ": " + (.Output // .Action))' \
				"$event_file" | tail -n 80 >&2 || true
		fi
	done
  failure_reason='Required Billing integration tests did not each pass without skip or failure'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi

finalize_detail
if [[ "$detail_produced" != true || "$detail_valid" != true || "$detail_bounded" != true || \
  "$detail_secret_free" != true || "$detail_assertions_valid" != true ]]; then
  [[ -n "$failure_reason" ]] || failure_reason='Go integration test did not produce valid bounded detail evidence'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi
if [[ "$test_status" != passed ]]; then exit "$test_exit_code"; fi
phase='test-complete'
exit 0
