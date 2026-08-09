# `@synara/cloud-agent-distribution`

Pinned Cloud Agent distribution shared by Synara and T3 Code. Its JavaScript
factory explicitly registers the allowlisted Providers; it also exports the
stdio client and deeply immutable source manifest, and does not modify either
host during installation.

The `cloud-agent-runtime` bin is emitted as a single bundled executable so a
host-side SHA-256 check covers the actual Runtime implementation rather than an
external-import shim. The current bin still enters the legacy Provider Host
handler; switching that entrypoint to the explicit Provider registry is a
release gate, not a completed property of this source slice.
