# Stage 6 candidate evidence bundle v3 (historical)

Candidate v3 was the ten-receipt Stripe/Billing-era contract. It remains readable only for offline audit and immutable
terminal `released`, `rejected`, or `rolled_back` records. Migration `000153_stage6_internal_self_hosted_release_gate.sql`
rejects any active v2/v3 candidate, and no new Candidate or Release ingestion path accepts it.

Use [Stage 6 candidate evidence bundle v5](stage-6-candidate-evidence-bundle-v5.md) for the internal self-hosted product.
V4 replaces the Billing receipt with exact-candidate internal Token, Provider cost, and platform-allocation evidence and
forbids payment semantics.
