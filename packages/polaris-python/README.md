# Polaris Python SDK

Private Stage 7 beta source for the official `polaris-agents` Python SDK. It currently exposes the
same forty-one codegen-ready public-beta operations as the TypeScript SDK, including bounded Project/Session reads with Usage, sanitized Provider capability, Execution-cancel, Turn interrupt/steer, checkpoint-resume and queued primary-operation projections, idempotent Project update/archive, Session model/history/lifecycle operations, response-loss-safe Artifact upload/download/delete, safe Interaction pages and user-input resolution, and durable SSH
provisioning create-and-poll. This package is intentionally
not published until package ownership, trusted publishing, deployment conformance, and release
approval are complete.

The build-only release path now produces and verifies a wheel and sdist alongside the TypeScript
tarball. Publication remains disabled until the PyPI project, OIDC trusted publisher, protected
environment, and release approval are configured outside the repository.

Use `targets.provision(...)` followed by `targets.wait_for_provisioning(...)` for SSH install,
upgrade, or revoke. The operation is a durable receipt; target registration by itself is not
compute readiness.

```python
from polaris_agents import Polaris

polaris = Polaris(api_key="syna_sa_...", base_url="https://api.example.com")
project = polaris.projects.create(
    tenant_id="tenant-id", organization_id="organization-id", name="CI automation"
)
session = polaris.sessions.create(project_id=project.data["id"], title="Fix CI", provider="codex")
session.send_turn(input_text="Fix the failing tests.")

for event in session.events():
    print(event["sequence"], event["eventType"])
```
