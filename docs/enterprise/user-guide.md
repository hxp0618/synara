# Enterprise user guide

This guide covers ordinary Synara users in an enterprise Tenant. Your administrator controls membership, identity policy,
Provider access, Region policy, quotas and retention. Controls that are not permitted for your role are omitted or read-only.

## Sign in and choose a Tenant

1. Open the Synara URL supplied by your administrator. Do not use a hostname from another environment; login cookies and
   SSO callbacks are bound to the deployed origin.
2. Sign in with the offered method. If the Tenant requires SSO, password or other non-SSO sessions cannot be used to enter
   it. Use the configured enterprise identity connection.
3. Accept a pending invitation when prompted, then select the correct Tenant and Organization in the context switcher.
   Always verify the visible Tenant before opening a repository or entering sensitive content.
4. If the Tenant is suspended, closed, deleting, or its trial has expired, Synara fails closed for new agent work. Contact
   the Tenant owner; repeatedly signing in does not bypass the lifecycle policy.

If an authenticated account has no Tenant, the gate can create an active Standard-profile Tenant and make it the current context. That
self-service path cannot choose an enterprise Plan or client-defined trial. Ask a Platform Admin for enterprise provisioning.

## Run and follow work

Create or open a project, then start a task from the chat composer. The Environment panel shows the active execution,
target, Worker, events and artifacts that your role may read. During live output, use Stop for an intentional interruption;
do not treat a reconnecting browser as proof that the remote execution stopped.

Provider failures such as authentication, quota, rate limiting or service availability are distinct from Synara execution
failures. Copy the visible error code and request ID for support, but never paste a Provider token, login cookie, presigned
artifact URL, prompt body or customer data into a ticket.

## Understand usage and cost

The Environment usage section explains the current Session and its Turns. It can include input, cached, output and reasoning
tokens, duration, network bytes, Provider cost and allocated platform cost when those sources reported data. Tenant Settings
shows the current reporting-period aggregate and soft quota warnings to authorized roles.

Each Turn separates Provider cost from allocated platform cost, names the allocation kinds, shows ingress/egress,
and then displays the per-currency total. The Session row aggregates those same components; it never adds different currencies
together. A Turn marked `updating` still has a non-final runtime report and its displayed total can increase.

Platform allocations identify their source as `estimated` or `actual`. When an immutable cloud-cost allocation exists for an
estimated slice, the actual amount replaces that estimate in Session and reporting-period totals; the two are never added
together. Tenant Settings labels the combined Provider and platform figure as a known internal subtotal, not an invoice.

- A value marked estimated is an internal cost estimate, not a payment request or invoice.
- Retried generations can make one Turn cost more than its final visible answer suggests.
- A soft 80% or 100% warning explains pressure but does not silently become a hard execution stop. Existing hard concurrency
  quota remains a separate admission rule.
- Missing Provider price or usage telemetry appears as unavailable and is excluded from known subtotals. An explicit `$0.00`
  is shown only when the Provider/runtime actually reported a zero cost.

The current enterprise-internal deployment has no payment or Checkout flow. Cost values support internal usage explanation
and cost-center allocation; they are not an external invoice. Synara must never ask you for card or payment details.

## Releases and required action

After an upgrade, Synara can show a one-time What's new card. The same history remains available under Settings → Release
history. Administrator action, breaking changes and urgent security notices appear ahead of ordinary feature highlights and
state the affected audience, action, first-announced time and effective time. Follow the linked migration guide before the
deadline; verify unexpected requests through your administrator or the published support route.

## Privacy, retention and help

Tenant policy controls retention, Legal Hold, export, erasure and execution Region placement. A Legal Hold can legitimately
block deletion. Region placement does not by itself prove that metadata, backups, logs, KMS, support processing or a Provider
stay in the same Region; rely on the signed deployment residency statement.

For help, give support the Tenant/Session/Execution identifiers, UTC time, visible error code, request ID and the action you
took. If your administrator grants Synara Support Access, the support session is reason-bound, time-limited, read-only,
Tenant-visible and revocable. Support should never ask you to disclose a Credential or approve direct database mutation.
