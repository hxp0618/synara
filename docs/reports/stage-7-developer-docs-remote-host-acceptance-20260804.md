# Stage 7 Developer Docs Remote-host Acceptance — 2026-08-04

## Result

- Static artifact build: **PASS**
- Remote-host extraction and loopback rendering: **PASS**
- Public-network reachability: **FAIL / not configured**
- Cleanup: **PASS**

The current Developer Docs artifact was packaged from `apps/developer-docs/dist` with SHA-256
`1fbfe6aa4e193d92a3248740ef192d7aa635dfe84ce380778f9a60fc947d96a0` and copied to the authorized
Debian VPS `188.239.23.134`. A uniquely named temporary directory and an otherwise unused high port were used;
no K3s, firewall, reverse-proxy, certificate or existing site configuration was changed.

On the remote host, `/`, `/getting-started/` and `/api-reference/` rendered successfully through a temporary
Python static server. A direct request from the development machine to the same public address and port timed out,
which is consistent with the VPS/cloud firewall not admitting that high port. This is not evidence of a deployed
public origin and the Stage 7 external-docs checklist row remains unchecked.

The server command was validated against its recorded PID before termination. The exact remote directory, uploaded
archive, process and local temporary archive were removed; a final remote socket/path check passed. The failed public
probe does not authorize opening the shared VPS firewall or modifying its K3s ingress.
