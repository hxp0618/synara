# Phase 1 remote server deployment

This runbook deploys the complete Synara server and web client as one Docker
Compose service on a Linux host. The Phase 1 objective is operational learning:

- whether a remote agent matches day-to-day coding habits;
- whether the current Git/worktree/PR workflow is sufficient;
- whether Codex and Claude login state stays usable on a remote host; and
- whether WebSocket, terminal, and long-running task behavior is reliable.

This deployment deliberately keeps the current domain architecture. It is a
single-server, single-replica runtime, not a distributed Synara control plane.

## Runtime layout

| Path                   | Purpose                                                                        | Persistence                        |
| ---------------------- | ------------------------------------------------------------------------------ | ---------------------------------- |
| `/home/synara`         | Linux home, Codex/Claude/GitHub/SSH credentials and user config                | Docker volume `synara-remote-home` |
| `/home/synara/.synara` | SQLite state, settings, logs, attachments, Codex overlay and managed worktrees | Included in the home volume        |
| `/workspace`           | User repositories operated on by agents                                        | Host bind mount                    |
| `/app`                 | Built Synara server and web client                                             | Replaced with the image            |

Back up `/home/synara` and the host workspace. Never treat the container
filesystem outside those mounts as durable.

## Prerequisites

- A Linux server with Docker Engine and Docker Compose v2.
- Enough disk for repositories, worktrees, provider history, and image builds.
- A TLS reverse proxy, Tailscale, or another trusted private network for access.
- The Synara repository checked out on the server.

Do not expose plain HTTP directly to the public internet. The Compose default
publishes Synara only on `127.0.0.1` so a same-host reverse proxy can reach it.

## First deployment

From the repository root:

```bash
cp deploy/remote/.env.example deploy/remote/.env
mkdir -p deploy/remote/workspace
chmod 700 deploy/remote/workspace
```

Generate a server access token and put it in `deploy/remote/.env`:

```bash
openssl rand -hex 32
```

Set `SYNARA_WORKSPACE_PATH` to an absolute host path for production. Match
`SYNARA_UID` and `SYNARA_GID` to the owner of that directory:

```bash
id -u
id -g
```

Validate, build, and start:

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml config --quiet
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml build
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml up -d
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml ps
```

Check the two health contracts:

```bash
curl -fsS http://127.0.0.1:3773/health
curl -fsS http://127.0.0.1:3773/ready
deploy/remote/smoke.sh http://127.0.0.1:3773
```

- `/health` is liveness and diagnostics. It remains HTTP 200 while exposing
  individual startup gates.
- `/ready` is traffic readiness. It returns HTTP 503 until every startup gate
  is ready, then HTTP 200 with `"status":"ready"`.

## Open the web client

Open the TLS URL with the access token in the URL fragment on the first load:

```text
https://synara.example.com/#token=<SYNARA_AUTH_TOKEN>
```

The web client keeps the token for the current browser tab so reloads continue
to work, then removes it from the visible URL. A new tab or browser session must
be opened with the fragment again. URL fragments are not sent to the reverse
proxy as part of the HTTP request.

If the UI shell loads but projects and threads stay empty, first confirm that
the browser WebSocket request reaches `/ws` and receives HTTP 101. A missing or
stale access token normally appears as a rejected WebSocket, not an empty
SQLite database.

## Provider login inside the container

Run provider login as the non-root `synara` user through Compose. The resulting
files live under `/home/synara` and survive image upgrades.

### Codex

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara codex --version
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara codex login --device-auth
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara codex login status
```

Complete the device flow in a browser. Codex source credentials persist under
`/home/synara/.codex`; Synara's generated overlay persists under
`/home/synara/.synara/codex-home-overlay`.

### Claude Code

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara claude --version
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara claude login
```

If the installed Claude CLI uses the interactive slash-command flow instead,
start `claude` in the container and run `/login`. Credentials persist under
`/home/synara/.claude` (or the CLI's configured directory).

After an image upgrade, verify both versions and login status before diagnosing
provider failures as Synara protocol problems.

## Git and pull request setup

Configure GitHub CLI and Git identity inside the container:

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara gh auth login
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara gh auth setup-git
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara git config --global user.name "Your Name"
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara git config --global user.email "you@example.com"
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara gh auth status
```

GitHub CLI state and global Git config persist in `/home/synara`.

For SSH instead of GitHub HTTPS auth:

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara ssh-keygen -t ed25519
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara ssh-keyscan github.com \
  | docker compose --env-file deploy/remote/.env \
      -f deploy/remote/docker-compose.yml exec -T synara sh -c 'cat >> ~/.ssh/known_hosts'
```

Review the fingerprint through a trusted source before accepting it. Do not
bake private keys or provider credentials into the image.

Clone repositories below `/workspace`:

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara \
  gh repo clone owner/repository /workspace/repository
```

If writes fail, correct ownership on the host and rebuild with matching
`SYNARA_UID`/`SYNARA_GID`. Avoid running Synara as root to mask permission
problems.

## Reverse proxy

WebSocket upgrades must reach `/ws`. Use TLS, disable response buffering, and
allow long idle/read timeouts so quiet terminals and long agent turns are not
closed by the proxy.

### Caddy

```caddyfile
synara.example.com {
  reverse_proxy 127.0.0.1:3773 {
    flush_interval -1
    transport http {
      read_timeout 24h
      write_timeout 24h
    }
  }
}
```

Caddy forwards WebSocket upgrades automatically.

### Nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

server {
    listen 443 ssl http2;
    server_name synara.example.com;

    location / {
        proxy_pass http://127.0.0.1:3773;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }
}
```

Keep proxy access logs protected. Although the recommended login uses a URL
fragment, provider callbacks and repository URLs can still contain sensitive
operational metadata.

## Upgrade, rollback, and backup

Before an upgrade:

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml stop
docker run --rm \
  -v synara-remote-home:/source:ro \
  -v "$PWD/backups:/backup" \
  alpine sh -c 'tar -C /source -czf /backup/synara-home-$(date +%Y%m%d-%H%M%S).tgz .'
```

Back up the host directory configured by `SYNARA_WORKSPACE_PATH` separately.
Then update and rebuild:

```bash
git pull --ff-only
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml build --pull
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml up -d
deploy/remote/smoke.sh http://127.0.0.1:3773
```

For rollback, check out the previous known-good revision, rebuild the previous
image, and start it against the same volumes. Restore a home backup only when a
state migration or data corruption requires it; rolling code back does not
normally require deleting current state.

## Diagnostics

### Container and server

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml ps
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml logs --tail=300 -f synara
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara \
  sh -lc 'id; pwd; ls -ld /home/synara /workspace; curl -fsS http://127.0.0.1:3773/ready'
```

`/health` can be live while `/ready` is 503. Inspect the false readiness field
before restarting repeatedly.

### Provider login

```bash
docker compose --env-file deploy/remote/.env \
  -f deploy/remote/docker-compose.yml exec synara sh -lc \
  'codex --version; codex login status; claude --version; ls -la ~/.codex ~/.claude'
```

Do not paste credential files into issues or logs.

### WebSocket and empty UI

- Confirm the browser uses `wss://.../ws` and receives HTTP 101.
- Reopen the site with `#token=<SYNARA_AUTH_TOKEN>` if the tab lost its token.
- Check that the proxy forwards `Upgrade` and `Connection` headers.
- Check browser and proxy idle timeouts before blaming long-task state.
- If `/ready` is healthy and `/ws` is 101, inspect the browser console and the
  server's orchestration snapshot path before changing SQLite.

### Terminal and PTY

- `docker compose exec synara sh -lc 'tty || true; ps -ef'` verifies the image
  has the expected process tools.
- Confirm the repository is writable by the `synara` uid/gid.
- A terminal dying exactly at a fixed interval usually indicates a proxy or
  load-balancer timeout.
- Tini is PID 1 so terminated child processes are reaped and stop signals reach
  the Node server.

### Long tasks and restart semantics

Synara persists transcripts, projections, settings, attachments, and terminal
history under the home volume. On server startup it reconciles turns left in a
stuck working state by the previous process exit.

It does **not** resume the previous in-memory provider process, PTY, shell child,
or arbitrary operating-system command. A restart kills those processes. After
restart, inspect the persisted transcript and explicitly continue or rerun the
operation. This distinction is part of the Phase 1 reliability evaluation.

## Phase 1 limitations

- Run exactly one replica. SQLite, in-memory provider sessions, PTYs, and local
  worktrees are not horizontally coordinated.
- No rolling multi-replica deployment or transparent live-task handoff.
- A container/server restart does not resume OS processes or provider streams.
- The Docker socket is not mounted by default. Agents cannot control the host
  Docker daemon unless an operator deliberately adds that high-privilege mount.
- The image contains a practical Node/Bun/Python/build tool baseline, not every
  language toolchain a repository might require.
- Provider and Git credentials are tied to the persistent remote Linux home;
  rotate and back them up as production credentials.

## Phase 1 acceptance checklist

Run the automated infrastructure baseline before the manual workflows below:

```bash
deploy/remote/acceptance.sh \
  --env-file deploy/remote/.env \
  --base-url https://synara.example.com \
  --idle-seconds 900 \
  --require-logins
```

This checks Compose state, liveness/readiness, unauthenticated and authenticated
WebSocket behavior, writable persistent paths, the remote toolchain, a real
`node-pty` shell, and provider login status. The access token is read inside the
container and is never printed. Run the controlled restart check separately,
after all active agent turns and terminals have drained:

```bash
deploy/remote/acceptance.sh \
  --env-file deploy/remote/.env \
  --base-url https://synara.example.com \
  --require-logins \
  --restart-check
```

The automated probe proves the server-side primitives. The browser, editing,
Git/PR, and real provider-turn checks remain intentionally operator-driven so
the evaluation reflects actual remote usage rather than only synthetic health.

Remote agent habits:

- [ ] Open several repositories from `/workspace` and complete real editing,
      search, build, and terminal workflows without relying on the local laptop.
- [ ] Confirm file permissions, tool availability, and copy/paste behavior match
      normal operator expectations.

Git and PR mode:

- [ ] Create an isolated worktree/branch, commit, push, and open a PR.
- [ ] Review PR checks/comments and continue work after reconnecting the browser.
- [ ] Confirm Git identity and `gh auth status` remain valid after image rebuild.

Codex and Claude login:

- [ ] Complete remote Codex device login and run a real Codex turn.
- [ ] Complete Claude login and run a real Claude turn.
- [ ] Restart and rebuild the container, then verify both providers remain logged
      in without copying credentials back into the image.

WebSocket, terminal, and long tasks:

- [ ] Keep an idle browser and terminal connected beyond proxy timeout windows.
- [ ] Run a long streaming agent task and verify output remains ordered and the
      UI reconnects after a temporary network interruption.
- [ ] Restart Synara during a controlled test and verify persisted history and
      stuck-turn reconciliation, while accepting that the old PTY/process dies.
- [ ] Back up and restore the home volume in a disposable environment.

## Local LAN mode

For short-lived access to a developer machine without Docker:

```bash
bun run build
TOKEN="$(openssl rand -hex 32)"
bun run --cwd apps/server start -- \
  --host 0.0.0.0 --port 3773 --auth-token "$TOKEN" --no-browser
```

Open `http://<trusted-lan-ip>:3773/#token=<TOKEN>`. Prefer a Tailnet IP over a
public interface, and do not reuse this development command as a production
service manager.

## CLI and environment reference

| CLI flag                | Environment variable  | Meaning                                    |
| ----------------------- | --------------------- | ------------------------------------------ |
| `--mode <web\|desktop>` | `SYNARA_MODE`         | Runtime mode                               |
| `--port <number>`       | `SYNARA_PORT`         | HTTP/WebSocket port                        |
| `--host <address>`      | `SYNARA_HOST`         | Bind interface                             |
| `--home-dir <path>`     | `SYNARA_HOME`         | Synara state base directory                |
| `--dev-url <url>`       | `VITE_DEV_SERVER_URL` | Development web URL                        |
| `--no-browser`          | `SYNARA_NO_BROWSER`   | Disable automatic browser launch           |
| `--auth-token <token>`  | `SYNARA_AUTH_TOKEN`   | Legacy remote WebSocket/asset access token |
