# Capacity and long-duration test

1. Copy the Stage 6 release checklist and freeze the candidate commit, image digests, Migration tail and approved demand
   forecast. Confirm the error budget permits the exercise and a rollback owner is present.
2. Use an isolated production-like environment with the production topology, PostgreSQL/object/KMS/Queue classes, Worker
   images, autoscaling limits, Region layout and external probes. Fixture Provider runs validate harness behavior only.
3. Start one continuous telemetry window. Execute steady peak, burst, Tenant hotspot/fairness, Control Plane rolling restart,
   Worker churn, bounded database-connection pressure, bounded Outbox backpressure and cooldown. Do not delete failed rows,
   restart Prometheus or reset counters between phases.
4. Abort destructive escalation before exhausting the database pool, Worker capacity or error budget. An abort remains a
   failed run with evidence; do not relabel it as a successful lower-load profile.
5. Export exact Prometheus range queries/results, external probes, database pool/query samples, Kubernetes resource/restart
   records, redacted logs, workload assertions and the operator summary. Store no secrets or customer payloads.
6. Put a path-only `synara.capacity-soak-evidence-draft.v1` beside the seven exported files and run
   `bun run stage6:capacity:prepare -- --evidence-root <attempt-root> --draft <draft-relative-path> --manifest-output
<new-manifest-relative-path> --receipt-output <new-receipt-relative-path>`. Do not hand-author hashes or reuse output
   names. Review every failed objective, no-data interval, planned/unplanned restart and Tenant fairness result with
   Operations and Engineering. The validator-only command is reserved for independently checking an existing manifest.
7. Import the exact receipt through Platform Admin → Capacity reviews as the release-candidate creator. Confirm the product
   displays the same Commit/environment, 24/72-hour duration, probe coverage, five phase projections and exact receipt hash.
8. Assign distinct, active `capacity.engineering` and `capacity.operations` Governance roles from an Operator Tenant Owner.
   Each operator records an immutable decision with a protected HTTPS evidence reference and the nonzero lowercase
   `sha256:<64-hex>` digest of the exact external evidence bytes reviewed. A URL without that byte binding is not an active
   decision. A rejection terminates the internal gate; one operator cannot satisfy both roles.
9. Attach the receipt, Platform Audit request IDs, external signatures/authority evidence and immutable evidence links to
   the copied GA checklist. Only a production/production-like duration, complete evidence, passing measurements and signed
   external approval can check the capacity/long-duration item. Internal `approved` only unlocks the exact-candidate Release
   gate and must never be described as the real capacity run having passed.

Migration `000145_stage6_capacity_approval_evidence_digests.sql` automatically supersedes historical URL-only decisions
and reopens their previously approved Capacity run for replacement review. Engineering and Operations must submit new
byte-bound decisions; Release candidates already in a forward state are revalidated fail-closed against both active
digests.
