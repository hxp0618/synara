export type DocSection = {
  id: string;
  title: string;
  body: string[];
  code?: string;
};

export type DocPage = {
  slug: string;
  navLabel: string;
  title: string;
  description: string;
  sections: DocSection[];
  next?: { slug: string; label: string; description: string };
};

export const quickstartCode = `import { Polaris } from "@polaris-agents/sdk";

const polaris = new Polaris({
  apiKey: process.env.POLARIS_API_KEY!,
  baseUrl: process.env.POLARIS_BASE_URL!,
});

const project = await polaris.projects.create(
  process.env.POLARIS_TENANT_ID!,
  process.env.POLARIS_ORGANIZATION_ID!,
  { name: "CI automation", visibility: "organization" },
);

const session = await polaris.sessions.create({
  projectId: project.data.id,
  title: "Fix CI",
  provider: "codex",
});

await session.sendTurn({
  inputText: "Find and fix the failing tests.",
  runtimeMode: "full-access",
  interactionMode: "default",
});

for await (const event of session.events()) {
  console.log(event.sequence, event.eventType, event.payload);
}`;

export const pages: DocPage[] = [
  {
    slug: "getting-started",
    navLabel: "Build your first agent workflow",
    title: "Build your first agent workflow",
    description:
      "Create a session, send a message to your agent, and stream durable events as the work completes.",
    sections: [
      {
        id: "setup",
        title: "Setup in 3 steps",
        body: [
          "Install @polaris-agents/sdk, create a server-side client with a Tenant-scoped owner/admin API Key, then create a Project and start its first Session.",
        ],
      },
      {
        id: "complete-example",
        title: "Complete example (TypeScript)",
        body: [
          "The SDK generates an Idempotency-Key for every mutation and resumes the event stream from its last durable sequence.",
        ],
        code: quickstartCode,
      },
      {
        id: "whats-happening",
        title: "What happens next",
        body: [
          "Session events are ordered by sequence. Save the latest processed sequence when the consumer commits its side effects; reconnect with afterSequence to resume without missing events.",
          "Approval requests arrive through the same stream. Resolve them with polaris.approvals.resolve and keep the mutation idempotency key stable if your application retries.",
        ],
      },
    ],
    next: {
      slug: "python-sdk",
      label: "Python SDK",
      description: "Run the same durable workflow from Python 3.11+.",
    },
  },
  {
    slug: "python-sdk",
    navLabel: "Python SDK",
    title: "Build agent workflows in Python",
    description:
      "The zero-runtime-dependency Python SDK shares the same complete forty-one-operation public-beta contract and conformance suite as TypeScript.",
    sections: [
      {
        id: "install",
        title: "Create a server-side client",
        body: [
          "Install polaris-agents in a Python 3.11+ service, load the API Key from your secret manager, and configure the Control Plane base URL explicitly.",
        ],
        code: `from polaris_agents import Polaris

polaris = Polaris(
    api_key=os.environ["POLARIS_API_KEY"],
    base_url=os.environ["POLARIS_BASE_URL"],
)`,
      },
      {
        id: "workflow",
        title: "Create, turn, and stream",
        body: [
          "Python exposes an idiomatic synchronous iterator while preserving the same durable sequence, replay suppression, connection-pool polling fallback, and fail-closed gap behavior.",
        ],
        code: `project = polaris.projects.create(
    tenant_id=os.environ["POLARIS_TENANT_ID"],
    organization_id=os.environ["POLARIS_ORGANIZATION_ID"],
    name="CI automation",
)

session = polaris.sessions.create(
    project_id=project.data["id"],
    title="Fix CI",
    provider="codex",
)

session.send_turn(input_text="Find and fix the failing tests.")
for event in session.events():
    print(event["sequence"], event["eventType"])`,
      },
      {
        id: "reliability",
        title: "Reliability matches TypeScript",
        body: [
          "Mutations create an idempotency key automatically and retry only with that stable key. 429 and retryable 5xx responses use Retry-After or bounded exponential backoff.",
          "The package currently remains private beta source. The repository can build and verify its wheel and sdist through the shared release workflow, but PyPI ownership, OIDC trusted-publisher registration, protected-environment approval, and deployed-Control-Plane acceptance remain open gates.",
        ],
      },
    ],
    next: {
      slug: "execution-targets",
      label: "BYO Execution Targets",
      description: "Register tenant-owned compute without leaking its configuration.",
    },
  },
  {
    slug: "execution-targets",
    navLabel: "BYO Execution Targets",
    title: "Register tenant-owned compute",
    description:
      "Tenant owners can idempotently register SSH, Docker, or Kubernetes targets and read a secret-free status projection.",
    sections: [
      {
        id: "authorization",
        title: "Use a dedicated Tenant-scoped key",
        body: [
          "Execution Target registration requires a Tenant-scoped API Key whose fixed owner or admin role grants Worker management. Organization-scoped agent_operator keys intentionally cannot mutate Tenant compute supply.",
          "Local targets cannot be created by Service Accounts. The external contract accepts only ssh, docker, and kubernetes.",
        ],
      },
      {
        id: "register",
        title: "Register exactly once",
        body: [
          "Pass the owning Organization, target kind, name, configuration, and non-secret capabilities. Keep the same idempotency key when retrying the same registration.",
          "Configuration is encrypted at rest and omitted from every response. Capabilities are public metadata, so secret-like keys are rejected.",
        ],
        code: `const target = await polaris.targets.create(
  process.env.POLARIS_TENANT_ID!,
  {
    organizationId: process.env.POLARIS_ORGANIZATION_ID!,
    kind: "ssh",
    name: "build-host-01",
    configuration: loadSSHConfigurationFromSecretManager(),
    capabilities: {},
  },
  { idempotencyKey: deployment.id },
);`,
      },
      {
        id: "provision",
        title: "Provision and wait for readiness",
        body: [
          "Provisioning is a durable asynchronous operation. Reuse the same idempotency key for one logical request, then poll the operation until it reaches succeeded or failed.",
          "The reconciler uses a leased claim, a stable Worker identity, and target fencing. Registration or SSH command success alone is not compute readiness; succeeded is committed only after exact Worker readiness.",
        ],
        code: `const accepted = await polaris.targets.provision(
  process.env.POLARIS_TENANT_ID!,
  target.data.id,
  "install",
  { idempotencyKey: deployment.id + ":install" },
);

const completed = await polaris.targets.waitForProvisioning(
  process.env.POLARIS_TENANT_ID!,
  target.data.id,
  accepted.data.id,
);`,
      },
    ],
    next: {
      slug: "authentication",
      label: "Authentication",
      description: "Create, rotate, scope, and revoke API Keys safely.",
    },
  },
  {
    slug: "authentication",
    navLabel: "Authentication",
    title: "Authenticate server-side requests",
    description:
      "Polaris API Keys are scoped Service Account tokens for server-to-server integrations.",
    sections: [
      {
        id: "api-keys",
        title: "API Keys",
        body: [
          "Create a Service Account in Tenant Settings, grant the api.access scope and a fixed tenant role, then copy the syna_sa_ token. The plaintext token is shown only when it is created or rotated.",
          "Send the token as Authorization: Bearer syna_sa_…. Never place it in browser code, URLs, logs, or source control.",
        ],
        code: `const polaris = new Polaris({
  apiKey: process.env.POLARIS_API_KEY!,
  baseUrl: process.env.POLARIS_BASE_URL!,
});`,
      },
      {
        id: "scope",
        title: "Tenant and role scope",
        body: [
          "Every key belongs to one Tenant and derives permissions from its fixed role. A path that names another Tenant is rejected. Machine principals never inherit the creator's user credentials or private Sessions.",
        ],
      },
      {
        id: "rotation",
        title: "Rotation and revocation",
        body: [
          "Deploy the replacement token before revoking the old token. Rotation and revocation take effect on the next request; open streams must reconnect and authenticate again.",
        ],
      },
    ],
    next: {
      slug: "idempotency-and-retries",
      label: "Idempotency and retries",
      description: "Make mutations safe across timeouts and transient failures.",
    },
  },
  {
    slug: "idempotency-and-retries",
    navLabel: "Idempotency and retries",
    title: "Retry mutations without duplicating work",
    description:
      "Idempotency keys bind a mutation result to the Tenant, machine principal, route, and request body.",
    sections: [
      {
        id: "idempotency",
        title: "One logical operation, one key",
        body: [
          "The TypeScript SDK creates a UUID idempotency key automatically. If your application supplies one, reuse it only for retries of the exact same request body.",
          "A replay returns the original result with Idempotency-Replayed: true. Reusing a key with a different body returns idempotency_conflict.",
        ],
        code: `await session.sendTurn(input, {
  idempotencyKey: job.id,
});`,
      },
      {
        id: "retry-policy",
        title: "Retry policy",
        body: [
          "The SDK retries network failures, 429, 500, 502, 503, and 504 responses only when the mutation carries an idempotency key. It honors Retry-After and otherwise uses bounded exponential backoff.",
        ],
      },
      {
        id: "rate-limits",
        title: "Rate limits",
        body: [
          "RateLimit-Limit, RateLimit-Remaining, and RateLimit-Reset describe the API Key's current minute window. A 429 response is also included in usage attribution.",
        ],
      },
    ],
    next: {
      slug: "streaming-events",
      label: "Streaming events",
      description: "Consume ordered agent progress with resumable SSE.",
    },
  },
  {
    slug: "streaming-events",
    navLabel: "Streaming events",
    title: "Consume a resumable Session event stream",
    description:
      "The Session SSE stream replays durable history and then follows new events in strict sequence order.",
    sections: [
      {
        id: "iterator",
        title: "Use the async iterator",
        body: [
          "session.events() reconnects after transient failures and requests only events after the latest sequence yielded to your code. If the server rejects a new SSE connection because its user or Tenant stream pool is full, the SDK automatically continues through bounded event-page polling.",
        ],
        code: `for await (const event of session.events({ afterSequence })) {
  await applyEvent(event);
  await checkpoints.save(event.sequence);
}`,
      },
      {
        id: "delivery",
        title: "Delivery semantics",
        body: [
          "Consumers must tolerate replay. Commit application side effects and the corresponding sequence checkpoint atomically where possible.",
          "The SDK suppresses replayed sequences and fails closed with PolarisSequenceGapError if the server skips a sequence. It does not silently hide a gap.",
          "Polling fallback uses the same afterSequence cursor and sequence-gap checks as SSE. Disable it with fallbackToPolling: false only when your application prefers to fail immediately on stream-pool saturation.",
        ],
      },
      {
        id: "execution-filter",
        title: "Filter one Execution without weakening sequence checks",
        body: [
          "Use session.eventsForExecution(executionId) in TypeScript or session.events_for_execution(execution_id) in Python when a consumer only needs one Execution. The SDK validates the complete Session sequence before filtering, so unrelated events do not create false gaps and real Session gaps still fail closed.",
        ],
        code: `for await (const event of session.eventsForExecution(executionId)) {
  await applyExecutionEvent(event);
}`,
      },
      {
        id: "forward-compatibility",
        title: "Unknown event types",
        body: [
          "Event payloads are forward compatible. Preserve unknown event types and their raw payload instead of rejecting or discarding them.",
        ],
      },
    ],
    next: {
      slug: "approvals",
      label: "Approvals",
      description: "Resolve durable human-in-the-loop decisions.",
    },
  },
  {
    slug: "approvals",
    navLabel: "Approvals",
    title: "Resolve execution approvals",
    description:
      "Approval requests are durable interactions projected into the Session event stream.",
    sections: [
      {
        id: "resolve",
        title: "Accept or decline",
        body: [
          "Read executionId and requestId from the approval event, apply your policy, and resolve the interaction once. Keep the idempotency key stable across retries.",
        ],
        code: `await polaris.approvals.resolve(
  event.executionId,
  event.requestId,
  "accept",
  { idempotencyKey: policyDecision.id },
);`,
      },
      {
        id: "reconciliation",
        title: "Reconcile races",
        body: [
          "A decision may race another operator or policy engine. Treat an already-resolved interaction as terminal, refresh current state, and do not infer state from a stale event alone.",
        ],
      },
    ],
    next: {
      slug: "webhooks",
      label: "Webhooks",
      description: "Verify signed, retryable thin Event deliveries.",
    },
  },
  {
    slug: "webhooks",
    navLabel: "Webhooks",
    title: "Receive signed Webhook events",
    description:
      "Polaris Webhooks deliver a minimal Event envelope with stable delivery identity and HMAC authentication.",
    sections: [
      {
        id: "delivery",
        title: "Delivery contract",
        body: [
          "Each subscribed Session Event creates an independent delivery. The JSON body contains only schemaVersion, deliveryId, eventId, type, sequence, Tenant/Organization/Project/Session/Execution IDs, and occurredAt. Prompt, Credential, Event payload, and endpoint configuration are never included.",
          "Use X-Polaris-Webhook-Id or Idempotency-Key to deduplicate. A successful 2xx response completes delivery; transient failures use bounded exponential retry and eventually enter the dead-letter queue for an authorized replay.",
        ],
      },
      {
        id: "signature",
        title: "Verify before processing",
        body: [
          "Read the raw request bytes. Reject stale X-Polaris-Webhook-Timestamp values, compute HMAC-SHA256 over timestamp + '.' + rawBody with the one-time whsec_ secret, and compare v1 signatures in constant time.",
        ],
        code: `import { createHmac, timingSafeEqual } from "node:crypto";

const expected = "v1=" + createHmac("sha256", process.env.POLARIS_WEBHOOK_SECRET!)
  .update(timestamp + ".")
  .update(rawBody)
  .digest("hex");

if (!timingSafeEqual(Buffer.from(expected), Buffer.from(signature))) {
  throw new Error("invalid Webhook signature");
}`,
      },
      {
        id: "operations",
        title: "Operate endpoints safely",
        body: [
          "Endpoint URLs must use HTTPS and cannot contain credentials, query parameters, or fragments. Polaris also blocks redirects and private, loopback, link-local, and reserved destinations.",
          "Rotate signing secrets after deploying dual-secret verification, and disable an endpoint before maintenance. Delivery diagnostics expose status and a redacted failure summary, never receiver response bodies.",
        ],
      },
    ],
    next: {
      slug: "errors",
      label: "Errors",
      description: "Branch on stable error codes and retain request IDs.",
    },
  },
  {
    slug: "errors",
    navLabel: "Errors",
    title: "Handle typed API errors",
    description:
      "The SDK maps the stable error envelope to PolarisError without treating message text as program logic.",
    sections: [
      {
        id: "typed-errors",
        title: "Branch on code",
        body: [
          "Use error.code and error.status for control flow. Record error.requestId when reporting a problem; messages are written for humans and may change.",
        ],
        code: `try {
  await session.sendTurn(input);
} catch (error) {
  if (error instanceof PolarisError && error.code === "developer_api_rate_limited") {
    scheduleRetry(error.retryAfterSeconds);
  } else {
    throw error;
  }
}`,
      },
      {
        id: "transport-errors",
        title: "Transport and sequence failures",
        body: [
          "PolarisTransportError means no valid API response was available. PolarisSequenceGapError means ordered stream processing cannot continue safely from the current cursor.",
        ],
      },
    ],
    next: {
      slug: "api-lifecycle",
      label: "API lifecycle",
      description: "Understand compatibility, deprecation, and version support.",
    },
  },
  {
    slug: "api-lifecycle",
    navLabel: "API lifecycle",
    title: "API compatibility and deprecation",
    description: "Polaris v1 is additive-only; breaking changes move to a new major API version.",
    sections: [
      {
        id: "compatibility",
        title: "Stable v1 compatibility",
        body: [
          "Public v1 contracts add fields and operations without changing the meaning of existing fields. Clients must ignore unknown response fields and preserve unknown Event types. Breaking request, response, authentication, or semantic changes require /v2.",
          "public-beta marks an operation whose compatibility surface can still change before GA. public-ga operations follow the full deprecation window.",
        ],
      },
      {
        id: "deprecation",
        title: "At least 12 months notice",
        body: [
          "A deprecated public-ga operation is announced in the changelog and documentation and returns Deprecation and Sunset response headers. Sunset is at least 12 months after the first public notice unless continued operation would create an urgent security risk.",
          "Polaris does not silently redirect a removed operation to behavior with different authorization, idempotency, or data semantics.",
        ],
      },
      {
        id: "sdk-support",
        title: "SDK and Control Plane compatibility",
        body: [
          "Each SDK package publishes minimumControlPlaneVersion metadata. TypeScript 0.1.0-beta.1 and Python 0.1.0b1 currently require Control Plane 0.1.0-beta.1 or newer.",
          "Upgrade the Control Plane before installing an SDK whose minimum version is newer. Registry publication and a hosted compatibility matrix remain release gates for the source beta.",
        ],
      },
    ],
  },
];

export const pageBySlug = new Map(pages.map((page) => [page.slug, page]));
