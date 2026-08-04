# Polaris Python examples

Run from the repository root after setting `POLARIS_API_KEY`, `POLARIS_BASE_URL`,
`POLARIS_PROJECT_ID`, and `CI_JOB_ID`:

```sh
PYTHONPATH=packages/polaris-python/src python3 examples/polaris/python/ci_fix_bot.py
```

The external CI job identity derives stable mutation idempotency keys. The API Key remains
server-side and must never be shipped in browser code.
