from __future__ import annotations

import hashlib
import os

from polaris_agents import Polaris


def stable_key(kind: str, external_id: str) -> str:
    digest = hashlib.sha256(f"{kind}:{external_id}".encode()).hexdigest()
    return f"ci-{digest[:48]}"


polaris = Polaris(
    api_key=os.environ["POLARIS_API_KEY"],
    base_url=os.environ["POLARIS_BASE_URL"],
)
job_id = os.environ["CI_JOB_ID"]
session = polaris.sessions.create(
    project_id=os.environ["POLARIS_PROJECT_ID"],
    title=f"Fix CI job {job_id}",
    provider="codex",
    idempotency_key=stable_key("session", job_id),
)
session.send_turn(
    input_text="Reproduce the failing CI job, implement the smallest safe fix, and run focused tests.",
    idempotency_key=stable_key("turn", job_id),
)

for event in session.events():
    print(event["sequence"], event["eventType"])
    if event["eventType"] in {"turn.completed", "execution.failed"}:
        break
