#!/usr/bin/env bash
set -euo pipefail

: "${POLARIS_BASE_URL:?Set POLARIS_BASE_URL to the Control Plane origin}"
: "${POLARIS_API_KEY:?Set POLARIS_API_KEY to a syna_sa_ Service Account key}"
: "${POLARIS_PROJECT_ID:?Set POLARIS_PROJECT_ID to an accessible Project UUID}"

for command in curl jq; do
  command -v "$command" >/dev/null || { printf 'Missing required command: %s\n' "$command" >&2; exit 1; }
done

base_url="${POLARIS_BASE_URL%/}"
authorization="Authorization: Bearer ${POLARIS_API_KEY}"

uuid_key() {
  if command -v uuidgen >/dev/null; then uuidgen | tr '[:upper:]' '[:lower:]'; return; fi
  od -An -N16 -tx1 /dev/urandom | tr -d ' \n'
}

urlencode() { jq -rn --arg value "$1" '$value | @uri'; }

create_session_body="$(jq -cn \
  --arg title "Polaris curl quickstart" \
  --arg provider "${POLARIS_PROVIDER:-codex}" \
  --arg executionTargetId "${POLARIS_EXECUTION_TARGET_ID:-}" \
  --arg providerCredentialId "${POLARIS_PROVIDER_CREDENTIAL_ID:-}" \
  '{title:$title,provider:$provider,visibility:"project"}
   + (if $executionTargetId == "" then {} else {executionTargetId:$executionTargetId} end)
   + (if $providerCredentialId == "" then {} else {providerCredentialId:$providerCredentialId} end)')"
session_response="$(curl --fail-with-body --silent --show-error \
  -X POST "${base_url}/v1/projects/$(urlencode "$POLARIS_PROJECT_ID")/sessions" \
  -H "$authorization" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: curl-session-$(uuid_key)" --data-binary "$create_session_body")"
session_id="$(jq -er '.id' <<<"$session_response")"

quickstart_prompt="${POLARIS_QUICKSTART_PROMPT:-Request one harmless command that needs approval, then finish.}"
turn_body="$(jq -cn --arg inputText "$quickstart_prompt" \
  '{inputText:$inputText,runtimeMode:"full-access",interactionMode:"default"}')"
curl --fail-with-body --silent --show-error \
  -X POST "${base_url}/v1/sessions/$(urlencode "$session_id")/turns" \
  -H "$authorization" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: curl-turn-$(uuid_key)" --data-binary "$turn_body" >/dev/null

printf 'Session %s created. Following the durable SSE stream...\n' "$session_id" >&2
curl --fail-with-body --silent --show-error --no-buffer --max-time 10 \
  "${base_url}/v1/sessions/$(urlencode "$session_id")/events/stream?afterSequence=0" \
  -H "$authorization" -H 'Accept: text/event-stream' || {
    status=$?
    if [[ $status -ne 28 ]]; then exit "$status"; fi
  }

deadline=$((SECONDS + 120))
approval_event=''
while (( SECONDS < deadline )); do
  page="$(curl --fail-with-body --silent --show-error \
    "${base_url}/v1/sessions/$(urlencode "$session_id")/events?afterSequence=0&limit=200" -H "$authorization")"
  approval_event="$(jq -cr '[.items[] | select(
    .eventType == "approval.requested" or
    (.eventType == "request.opened" and (.payload.requestType | type) == "string" and (.payload.requestType | endswith("_approval")))
  )][-1] // empty' <<<"$page")"
  [[ -n "$approval_event" ]] && break
  sleep 1
done
[[ -n "$approval_event" ]] || { printf 'No approval Event arrived within 120 seconds.\n' >&2; exit 1; }

execution_id="$(jq -er '.executionId' <<<"$approval_event")"
request_id="$(jq -er '.payload.requestId' <<<"$approval_event")"
decision_body='{"decision":"accept"}'
curl --fail-with-body --silent --show-error \
  -X POST "${base_url}/v1/executions/$(urlencode "$execution_id")/approvals/$(urlencode "$request_id")/resolve" \
  -H "$authorization" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: curl-approval-$(uuid_key)" --data-binary "$decision_body" >/dev/null

printf 'Resolved approval %s for Execution %s.\n' "$request_id" "$execution_id"

deadline=$((SECONDS + 120))
terminal_event=''
while (( SECONDS < deadline )); do
  page="$(curl --fail-with-body --silent --show-error \
    "${base_url}/v1/sessions/$(urlencode "$session_id")/events?afterSequence=0&limit=200" -H "$authorization")"
  terminal_event="$(jq -cr --arg executionId "$execution_id" '[.items[] | select(
    .executionId == $executionId and (
      .eventType == "execution.completed" or
      .eventType == "execution.failed" or
      .eventType == "execution.cancelled" or
      .eventType == "execution.interrupted"
    )
  )][-1] // empty' <<<"$page")"
  [[ -n "$terminal_event" ]] && break
  sleep 1
done
[[ -n "$terminal_event" ]] || { printf 'Execution did not reach a terminal Event within 120 seconds.\n' >&2; exit 1; }
terminal_type="$(jq -er '.eventType' <<<"$terminal_event")"
[[ "$terminal_type" == "execution.completed" ]] || {
  printf 'Execution ended with %s instead of execution.completed.\n' "$terminal_type" >&2
  exit 1
}
printf 'Execution %s completed. Quickstart passed.\n' "$execution_id"
