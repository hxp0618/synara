#!/bin/sh
set -eu

cat >/dev/null
printf 'agentd artifact payload\n' > result.txt
printf '%s\n' '{"type":"event","eventType":"runtime.output.delta","payload":{"text":"agentd runner started"}}'
printf '%s\n' '{"type":"artifact","artifact":{"path":"result.txt","kind":"generated_file","originalName":"result.txt","contentType":"text/plain"}}'
printf '%s\n' '{"type":"result","output":{"summary":"agentd runner completed"},"providerResumeCursor":"agentd-test-cursor"}'
