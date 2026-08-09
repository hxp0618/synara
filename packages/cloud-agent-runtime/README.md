# `@synara/cloud-agent-runtime`

The default entrypoint exposes only the app-neutral Runtime and stdio client.
The temporary `legacy-provider-host` subpath exists solely for first-party
Provider migration and is not part of the stable host ABI.

Portable Cloud Agent runtime, explicit provider registry, stdio transport, and the one-minor Provider Host v2 compatibility implementation. Provider packages are registered explicitly by the distribution or host.

The compatibility implementation still contains the historical Codex/Claude
execution code. Moving those implementations and their upstream dependencies
into the Provider packages is a release gate; package names alone do not imply
that physical separation is complete.
