# Stage 6 engineering source gate

This aggregate gate runs the current-source compatibility, enterprise documentation, operations UI, route-authentication
and Tenant-isolation validators, followed by every Stage 6 Python validator test suite:

```bash
bun run stage6:engineering:check
```

The final assessment is always `stage6-engineering-source-gates-passed-not-ga-approved`. It proves that checked-in source
and validator contracts are internally consistent; it cannot approve production billing, SLO, recovery, incident, capacity,
penetration, native package, candidate-bundle, data-residency, compliance or release evidence.
