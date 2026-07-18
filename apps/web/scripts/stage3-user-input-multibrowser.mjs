import assert from "node:assert/strict";
import { mkdir, readFile } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";

import { chromium } from "playwright";

const webOrigin = requiredURL("SYNARA_STAGE3_WEB_ORIGIN");
const controlPlaneOrigin = requiredURL("SYNARA_STAGE3_CONTROL_PLANE_URL");
const workerRegistrationToken = requiredEnvironment("SYNARA_WORKER_REGISTRATION_TOKEN");
const manualMode = process.env.SYNARA_STAGE3_MANUAL === "1";
const evidenceDirectory =
  process.env.SYNARA_STAGE3_EVIDENCE_DIR?.trim() ||
  `/tmp/synara-stage3-user-input-${Date.now().toString(36)}`;
const runID = `user-input-${Date.now().toString(36)}-${randomUUID().slice(0, 8)}`;
const userEmail =
  process.env.SYNARA_STAGE3_USER_EMAIL?.trim() || `stage3-user-input-${runID}@example.invalid`;
const displayName = "Stage 3 User Input Acceptance";
const requestTimeoutMilliseconds = 20_000;
const browserWaitMilliseconds = 30_000;
const manualWaitMilliseconds = positiveIntegerEnvironment(
  "SYNARA_STAGE3_MANUAL_TIMEOUT_MS",
  300_000,
);

function requiredEnvironment(name) {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set; pass the value through the process environment.`);
  }
  return value;
}

function requiredURL(name) {
  const value = requiredEnvironment(name);
  const url = new URL(value);
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error(`${name} must be an HTTP(S) origin.`);
  }
  return url.origin;
}

function positiveIntegerEnvironment(name, fallback) {
  const raw = process.env[name]?.trim();
  if (!raw) return fallback;
  if (!/^\d+$/u.test(raw)) {
    throw new Error(`${name} must be a positive integer.`);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive safe integer.`);
  }
  return value;
}

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function parseJSON(text) {
  if (!text.trim()) return null;
  return JSON.parse(text);
}

function safeProblemCode(text) {
  try {
    const problem = parseJSON(text);
    const code = problem?.error?.code ?? problem?.code;
    return typeof code === "string" && /^[a-z0-9_.-]{1,128}$/iu.test(code) ? code : null;
  } catch {
    return null;
  }
}

function responseFailure(method, path, status, text) {
  const code = safeProblemCode(text);
  return new Error(`${method} ${path} returned ${status}${code ? ` (${code})` : ""}.`);
}

async function apiJSON(api, method, path, input, expectedStatuses = [200], headers = {}) {
  const response = await api.fetch(path, {
    method,
    ...(input === undefined ? {} : { data: input }),
    headers: { Accept: "application/json", ...headers },
    timeout: requestTimeoutMilliseconds,
  });
  const text = await response.text();
  if (!expectedStatuses.includes(response.status())) {
    throw responseFailure(method, path, response.status(), text);
  }
  return { status: response.status(), headers: response.headers(), body: parseJSON(text) };
}

function createProductAPI() {
  const cookies = new Map();
  return {
    async fetch(path, options) {
      const response = await fetch(new URL(path, webOrigin), {
        method: options.method,
        headers: {
          ...options.headers,
          ...(options.data === undefined ? {} : { "Content-Type": "application/json" }),
          ...(cookies.size > 0
            ? {
                Cookie: [...cookies].map(([name, value]) => `${name}=${value}`).join("; "),
              }
            : {}),
        },
        body: options.data === undefined ? undefined : JSON.stringify(options.data),
        signal: AbortSignal.timeout(options.timeout ?? requestTimeoutMilliseconds),
      });
      const setCookieHeaders =
        typeof response.headers.getSetCookie === "function"
          ? response.headers.getSetCookie()
          : [response.headers.get("set-cookie")].filter(Boolean);
      for (const setCookie of setCookieHeaders) {
        const pair = setCookie.split(";", 1)[0] ?? "";
        const separator = pair.indexOf("=");
        if (separator > 0) {
          cookies.set(pair.slice(0, separator), pair.slice(separator + 1));
        }
      }
      return {
        status: () => response.status,
        headers: () => Object.fromEntries(response.headers.entries()),
        text: () => response.text(),
      };
    },
    cookies() {
      assert.ok(cookies.size > 0, "product API login cookie is not available");
      return [...cookies].map(([name, value]) => ({ name, value }));
    },
    async dispose() {},
  };
}

async function workerJSON(token, method, path, input, expectedStatuses = [200], workerRequestID) {
  const response = await fetch(new URL(path, controlPlaneOrigin), {
    method,
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
      ...(workerRequestID ? { "X-Request-ID": workerRequestID } : {}),
    },
    body: input === undefined ? undefined : JSON.stringify(input),
    signal: AbortSignal.timeout(requestTimeoutMilliseconds),
  });
  const text = await response.text();
  if (!expectedStatuses.includes(response.status)) {
    throw responseFailure(method, path, response.status, text);
  }
  return { status: response.status, headers: response.headers, body: parseJSON(text) };
}

async function eventually(label, operation, timeoutMilliseconds = browserWaitMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const value = await operation();
      if (value !== false && value !== null && value !== undefined) return value;
    } catch (error) {
      lastError = error;
    }
    await delay(100);
  }
  throw new Error(
    `${label} did not converge within ${timeoutMilliseconds}ms${
      lastError instanceof Error ? `: ${lastError.message}` : ""
    }`,
  );
}

async function providerCapabilities(workerVersion) {
  const catalogURL = new URL(
    "../../../packages/contracts/src/providerCapabilityCatalog.json",
    import.meta.url,
  );
  const catalog = JSON.parse(await readFile(catalogURL, "utf8"));
  const providers = Object.fromEntries(
    catalog.providers.map((entry) => {
      const remote = entry.supportTier !== "local-only";
      const runtimeVersion = entry.runtimePolicy.compatibleRange.minimumInclusive;
      return [
        entry.provider,
        {
          protocolVersion: { major: 2, minor: 1 },
          hostBuildVersion: "stage3-user-input-live-fixture-v1",
          maximumCommandBytes: 2 * 1024 * 1024,
          maximumMessageBytes: 1024 * 1024,
          runtimeEventVersions: { minimum: 2, maximum: 2 },
          credentialDeliveryModes: remote ? ["anonymous-fd"] : [],
          resumeStrategies: remote ? ["native-cursor", "authoritative-history"] : [],
          capabilityDescriptor: {
            provider: entry.provider,
            supportTier: entry.supportTier,
            adapterVersion: entry.adapterVersion,
            ...(entry.provider === "codex" ? { providerCliVersion: runtimeVersion } : {}),
            runtime: {
              kind: entry.runtimePolicy.kind,
              name: entry.runtimePolicy.name,
              version: runtimeVersion,
              available: true,
              versionSource: entry.runtimePolicy.versionSource,
              compatibleRange: entry.runtimePolicy.compatibleRange,
              compatible: true,
            },
            releasePolicy: {
              requiresExplicitEnablement: entry.supportTier === "experimental",
              enabled: true,
            },
            capabilities: entry.capabilities,
          },
        },
      ];
    }),
  );
  return {
    workerRuntime: {
      workerBuildVersion: workerVersion,
      workerProtocolMinimum: 2,
      workerProtocolMaximum: 2,
      runtimeEventMinimum: 2,
      runtimeEventMaximum: 2,
      operatingSystem: process.platform,
      architecture: process.arch,
    },
    providerHost: {
      protocolVersion: { major: 2, minor: 1 },
      legacy: false,
      providers,
    },
  };
}

async function login(api) {
  const result = await apiJSON(
    api,
    "POST",
    "/v1/auth/dev-login",
    { email: userEmail, displayName },
    [200],
  );
  assert.equal(result.body.authenticated, true);
  assert.ok(result.body.user.activeTenantId);
  return result.body;
}

async function createLiveExecution(productAPI) {
  const loginState = await login(productAPI);
  const tenantID = loginState.user.activeTenantId;
  const organizations = await apiJSON(
    productAPI,
    "GET",
    `/v1/tenants/${tenantID}/organizations`,
    undefined,
  );
  const organization = organizations.body.items.find((item) => item.status === "active");
  assert.ok(organization?.id, "dev bootstrap must expose an active organization");

  const project = (
    await apiJSON(
      productAPI,
      "POST",
      `/v1/tenants/${tenantID}/organizations/${organization.id}/projects`,
      {
        name: `Stage 3 user input ${runID}`,
        repositoryUrl: null,
        defaultBranch: "main",
        visibility: "organization",
      },
      [201],
      { "Idempotency-Key": `${runID}-project` },
    )
  ).body;

  const session = (
    await apiJSON(
      productAPI,
      "POST",
      `/v1/projects/${project.id}/sessions`,
      {
        title: `Stage 3 structured input ${runID}`,
        visibility: "project",
        provider: "codex",
        model: "gpt-5.5",
        providerCredentialId: null,
        executionTargetId: null,
      },
      [201],
      { "Idempotency-Key": `${runID}-session` },
    )
  ).body;

  const workerVersion = "stage3-user-input-live-v1";
  const registration = await workerJSON(
    workerRegistrationToken,
    "POST",
    "/v1/workers/register",
    {
      executionTargetId: session.executionTargetId,
      targetKind: "local",
      instanceUid: randomUUID(),
      clusterId: runID,
      namespace: "stage3-user-input",
      podName: `worker-${runID}`,
      version: workerVersion,
      protocolVersion: 2,
      capabilities: await providerCapabilities(workerVersion),
      leaseSupported: true,
      fencingSupported: true,
    },
    [201],
  );
  assert.equal(registration.body.worker.compatibilityStatus, "compatible");

  const turn = (
    await apiJSON(
      productAPI,
      "POST",
      `/v1/sessions/${session.id}/turns`,
      {
        inputText: "Exercise live multi-browser Structured User Input convergence.",
        runtimeMode: "full-access",
        interactionMode: "default",
      },
      [201],
      { "Idempotency-Key": `${runID}-turn` },
    )
  ).body;

  const workerToken = registration.body.token;
  const executionID = await eventually("turn execution identity", async () => {
    const events = (
      await apiJSON(
        productAPI,
        "GET",
        `/v1/sessions/${session.id}/events?afterSequence=0&limit=100`,
        undefined,
      )
    ).body.items;
    const created = events.find(
      (event) => event.eventType === "turn.created" && event.payload?.turnId === turn.id,
    );
    return created?.executionId ?? created?.payload?.executionId ?? false;
  });
  const claim = await eventually("execution claim", async () => {
    const result = await workerJSON(
      workerToken,
      "POST",
      "/v1/workers/executions/claim",
      {
        executionTargetId: session.executionTargetId,
        targetKind: "local",
        executionId: executionID,
      },
      [200],
      `${runID}-claim-${randomUUID()}`,
    );
    return result.body.execution ? result.body : false;
  });
  assert.equal(claim.execution.turnId, turn.id);

  const lease = {
    tenantId: claim.lease.tenantId,
    generation: claim.lease.generation,
    leaseToken: claim.lease.leaseToken,
  };
  await workerJSON(
    workerToken,
    "POST",
    `/v1/workers/executions/${claim.execution.id}/workspace/ready`,
    lease,
    [200],
    `${runID}-workspace-ready`,
  );
  await workerJSON(
    workerToken,
    "POST",
    `/v1/workers/executions/${claim.execution.id}/start`,
    lease,
    [200],
    `${runID}-start`,
  );

  return {
    tenantID,
    organization,
    project,
    session,
    turn,
    execution: claim.execution,
    worker: registration.body.worker,
    workerToken,
    lease,
  };
}

function startLeaseKeeper(state) {
  let stopped = false;
  let timer = null;
  let failure = null;
  let inFlight = Promise.resolve();
  let sequence = 0;

  const schedule = () => {
    if (stopped) return;
    timer = setTimeout(() => {
      inFlight = workerJSON(
        state.workerToken,
        "POST",
        `/v1/workers/executions/${state.execution.id}/renew`,
        { ...state.lease, providerResumeCursor: null },
        [200],
        `${runID}-renew-${sequence++}-${randomUUID()}`,
      )
        .catch((error) => {
          failure = error;
        })
        .finally(schedule);
    }, 8_000);
  };
  schedule();

  return {
    check() {
      if (failure) throw failure;
    },
    async stop() {
      stopped = true;
      if (timer) clearTimeout(timer);
      await inFlight;
      if (failure) throw failure;
    },
  };
}

async function appendRuntimeEvent(state, eventType, payload, label) {
  const result = await workerJSON(
    state.workerToken,
    "POST",
    `/v1/workers/executions/${state.execution.id}/events`,
    {
      ...state.lease,
      eventId: randomUUID(),
      eventVersion: 2,
      eventType,
      payload,
      occurredAt: new Date().toISOString(),
    },
    [201],
    `${runID}-event-${label}`,
  );
  return result.body;
}

async function appendUserInput(state, requestID, questions) {
  await appendRuntimeEvent(
    state,
    "user-input.requested",
    { requestId: requestID, questions },
    requestID,
  );
}

async function listPending(productAPI, sessionID) {
  return (await apiJSON(productAPI, "GET", `/v1/sessions/${sessionID}/interactions`, undefined))
    .body;
}

async function listInteractions(productAPI, executionID) {
  return (await apiJSON(productAPI, "GET", `/v1/executions/${executionID}/interactions`, undefined))
    .body.items;
}

async function waitForInteraction(productAPI, state, requestID, status) {
  return eventually(`interaction ${requestID} -> ${status}`, async () => {
    const interactions = await listInteractions(productAPI, state.execution.id);
    const interaction = interactions.find((item) => item.requestId === requestID);
    return interaction?.status === status ? interaction : false;
  });
}

async function consumeResolution(state, requestID, expectedAnswers) {
  const delivery = await eventually(`resolution delivery ${requestID}`, async () => {
    const result = await workerJSON(
      state.workerToken,
      "POST",
      `/v1/workers/executions/${state.execution.id}/interaction-resolutions/pull`,
      { ...state.lease, limit: 20 },
    );
    return result.body.items.find((item) => item.requestId === requestID) ?? false;
  });
  assert.deepEqual(delivery.resolution.answers, expectedAnswers);
  const deliveryInput = {
    ...state.lease,
    resolutionCommandId: delivery.commandId,
  };
  await workerJSON(
    state.workerToken,
    "POST",
    `/v1/workers/executions/${state.execution.id}/interaction-resolutions/${delivery.interactionId}/delivered`,
    deliveryInput,
    [200],
    `${runID}-${requestID}-delivered`,
  );
  await workerJSON(
    state.workerToken,
    "POST",
    `/v1/workers/executions/${state.execution.id}/interaction-resolutions/${delivery.interactionId}/acknowledged`,
    deliveryInput,
    [200],
    `${runID}-${requestID}-acknowledged`,
  );
  return delivery;
}

async function completeExecution(productAPI, state, leaseKeeper, outputText) {
  await appendRuntimeEvent(
    state,
    "content.delta",
    { streamKind: "assistant_text", delta: outputText },
    "terminal-output",
  );
  await leaseKeeper.stop();
  await workerJSON(
    state.workerToken,
    "POST",
    `/v1/workers/executions/${state.execution.id}/complete`,
    { ...state.lease, providerResumeCursor: null, output: { text: outputText } },
    [200],
    `${runID}-complete`,
  );
  return eventually("authoritative terminal event", async () => {
    const page = (
      await apiJSON(
        productAPI,
        "GET",
        `/v1/sessions/${state.session.id}/events?afterSequence=0&limit=200`,
        undefined,
      )
    ).body;
    const terminalEvents = page.items.filter(
      (event) =>
        event.executionId === state.execution.id &&
        [
          "execution.completed",
          "execution.failed",
          "execution.cancelled",
          "execution.interrupted",
        ].includes(event.eventType),
    );
    return terminalEvents.length === 1 ? { page, terminalEvents } : false;
  });
}

function composer(page) {
  return page.locator('form[data-chat-composer-form="true"]');
}

function resolvePath(state, requestID) {
  return `/v1/executions/${state.execution.id}/user-input/${encodeURIComponent(requestID)}/resolve`;
}

function responseMatches(response, path) {
  return response.request().method() === "POST" && new URL(response.url()).pathname === path;
}

async function responseProblem(response) {
  try {
    return parseJSON(await response.text());
  } catch {
    return null;
  }
}

async function loginBrowserContext(context, productAPI) {
  await context.addCookies(
    productAPI.cookies().map((cookie) => ({
      name: cookie.name,
      value: cookie.value,
      url: webOrigin,
      httpOnly: true,
      sameSite: "Lax",
    })),
  );
}

async function openSessionPage(browser, productAPI, state, consoleIssues, label) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 } });
  await loginBrowserContext(context, productAPI);
  const page = await context.newPage();
  page.on("console", (message) => {
    if (message.type() === "error" || message.type() === "warning") {
      const text = message.text();
      const expectedConflict =
        text === "Failed to load resource: the server responded with a status of 409 (Conflict)" ||
        /idempotency_(?:conflict|in_progress)|interaction_(?:resolution_conflict|not_pending)/iu.test(
          text,
        );
      consoleIssues.push({
        page: label,
        type: message.type(),
        category: expectedConflict ? "expected-conflict" : "unexpected",
        digest: createHash("sha256").update(text).digest("hex").slice(0, 16),
      });
    }
  });
  await page.goto(`${webOrigin}/${state.session.id}`, { waitUntil: "domcontentloaded" });
  await composer(page).waitFor({ state: "visible", timeout: browserWaitMilliseconds });
  assert.equal(new URL(page.url()).pathname, `/${state.session.id}`);
  assert.ok((await page.title()).trim().length > 0);
  assert.ok((await page.locator("body").innerText()).trim().length > 20);
  assert.equal(
    await page.locator("vite-error-overlay, [data-vite-error-overlay], nextjs-portal").count(),
    0,
  );
  return { context, page };
}

async function waitForQuestion(page, questionText) {
  const locator = composer(page).getByText(questionText, { exact: true });
  await locator.waitFor({ state: "visible", timeout: browserWaitMilliseconds });
  return locator;
}

async function waitForQuestionGone(page, questionText) {
  await composer(page)
    .getByText(questionText, { exact: true })
    .waitFor({ state: "detached", timeout: browserWaitMilliseconds });
}

async function assertSessionStreamBannerHidden(page) {
  for (const title of ["Reconnecting to Session Events", "Session Event stream unavailable"]) {
    await eventually(
      `${title} to hide after authoritative convergence`,
      async () => {
        const titleLocator = page.getByText(title, { exact: true });
        if ((await titleLocator.count()) === 0) return true;
        return (
          (await titleLocator
            .locator("xpath=ancestor::div[@aria-hidden='true' and @inert][1]")
            .count()) === 1
        );
      },
      5_000,
    );
  }
}

function assertConsoleHealth(consoleIssues) {
  const relevant = consoleIssues.filter((issue) => issue.category !== "expected-conflict");
  assert.deepEqual(relevant, [], `relevant browser console issues: ${JSON.stringify(relevant)}`);
}

async function runAutomated(productAPI, state, leaseKeeper) {
  await mkdir(evidenceDirectory, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  const consoleIssues = [];
  const pageAState = await openSessionPage(browser, productAPI, state, consoleIssues, "A");
  const pageBState = await openSessionPage(browser, productAPI, state, consoleIssues, "B");
  const pageA = pageAState.page;
  const pageB = pageBState.page;

  try {
    const raceRequestID = `${runID}-race`;
    const raceQuestion = "Choose the authoritative competing answer.";
    await appendUserInput(state, raceRequestID, [
      {
        id: "race-choice",
        header: "Competing resolve",
        question: raceQuestion,
        options: [
          { label: "Continue", description: "Submit the first competing answer." },
          { label: "Stop", description: "Submit the second competing answer." },
        ],
        multiSelect: true,
      },
    ]);
    const pendingRace = await eventually("race pending snapshot", async () => {
      const pending = await listPending(productAPI, state.session.id);
      const item = pending.items.find((candidate) => candidate.requestId === raceRequestID);
      return item ? { pending, item } : false;
    });
    assert.deepEqual(pendingRace.item.payload.questions[0], {
      id: "race-choice",
      header: "Competing resolve",
      question: raceQuestion,
      options: [
        { label: "Continue", description: "Submit the first competing answer." },
        { label: "Stop", description: "Submit the second competing answer." },
      ],
      multiSelect: true,
    });
    await Promise.all([waitForQuestion(pageA, raceQuestion), waitForQuestion(pageB, raceQuestion)]);
    await Promise.all([
      composer(pageA)
        .getByRole("button", { name: /Continue/u })
        .click(),
      composer(pageB).getByRole("button", { name: /Stop/u }).click(),
    ]);
    const racePath = resolvePath(state, raceRequestID);
    const responseAPromise = pageA.waitForResponse(
      (response) => responseMatches(response, racePath),
      { timeout: browserWaitMilliseconds },
    );
    const responseBPromise = pageB.waitForResponse(
      (response) => responseMatches(response, racePath),
      { timeout: browserWaitMilliseconds },
    );
    await Promise.all([
      composer(pageA).getByRole("button", { name: "Submit answers", exact: true }).click(),
      composer(pageB).getByRole("button", { name: "Submit answers", exact: true }).click(),
    ]);
    const [raceResponseA, raceResponseB] = await Promise.all([responseAPromise, responseBPromise]);
    const raceStatuses = [raceResponseA.status(), raceResponseB.status()].sort(
      (left, right) => left - right,
    );
    assert.deepEqual(raceStatuses, [200, 409]);
    const raceProblems = await Promise.all(
      [raceResponseA, raceResponseB]
        .filter((response) => response.status() === 409)
        .map(responseProblem),
    );
    const raceProblemCodes = raceProblems.map(
      (problem) => problem?.error?.code ?? problem?.code ?? null,
    );
    assert.ok(
      raceProblemCodes.every((code) =>
        [
          "idempotency_conflict",
          "idempotency_in_progress",
          "interaction_resolution_conflict",
          "interaction_not_pending",
        ].includes(code),
      ),
      `unexpected competing resolution problem: ${JSON.stringify(raceProblems)}`,
    );
    await Promise.all([
      waitForQuestionGone(pageA, raceQuestion),
      waitForQuestionGone(pageB, raceQuestion),
    ]);
    const raceInteraction = await waitForInteraction(productAPI, state, raceRequestID, "resolved");
    const raceAnswer = raceInteraction.resolution.answers["race-choice"];
    assert.ok(
      Array.isArray(raceAnswer) &&
        raceAnswer.length === 1 &&
        (raceAnswer[0] === "Continue" || raceAnswer[0] === "Stop"),
    );
    await consumeResolution(state, raceRequestID, { "race-choice": raceAnswer });

    const staleRequestID = `${runID}-stale-timer`;
    const staleQuestion = "Choose before the stale timer can submit.";
    await appendUserInput(state, staleRequestID, [
      {
        id: "stale-choice",
        header: "Stale timer",
        question: staleQuestion,
        options: [
          { label: "Continue", description: "Resolve authoritatively from the other page." },
          { label: "Stop", description: "Schedule an obsolete local auto-submit." },
        ],
        multiSelect: false,
      },
    ]);
    const stalePending = await eventually("stale timer pending snapshot", async () => {
      const pending = await listPending(productAPI, state.session.id);
      return pending.items.find((candidate) => candidate.requestId === staleRequestID) ?? false;
    });
    await Promise.all([
      waitForQuestion(pageA, staleQuestion),
      waitForQuestion(pageB, staleQuestion),
    ]);
    const stalePath = resolvePath(state, staleRequestID);
    let stalePageBRequestCount = 0;
    const countStalePageBRequest = (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === stalePath) {
        stalePageBRequestCount += 1;
      }
    };
    pageB.on("request", countStalePageBRequest);
    await composer(pageB).getByRole("button", { name: /Stop/u }).click();
    const staleDirectResult = await pageA.evaluate(
      async ({ path, idempotencyKey }) => {
        const response = await fetch(path, {
          method: "POST",
          credentials: "include",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/json",
            "Idempotency-Key": idempotencyKey,
          },
          body: JSON.stringify({ answers: { "stale-choice": "Continue" } }),
        });
        return { status: response.status, body: await response.json() };
      },
      {
        path: stalePath,
        idempotencyKey: `${runID}-${stalePending.id}-authoritative-resolve`,
      },
    );
    assert.equal(staleDirectResult.status, 200);
    await Promise.all([
      waitForQuestionGone(pageA, staleQuestion),
      waitForQuestionGone(pageB, staleQuestion),
    ]);
    await delay(500);
    pageB.off("request", countStalePageBRequest);
    assert.equal(stalePageBRequestCount, 0, "the obsolete page timer submitted after unmount");
    const staleInteraction = await waitForInteraction(
      productAPI,
      state,
      staleRequestID,
      "resolved",
    );
    assert.deepEqual(staleInteraction.resolution.answers, { "stale-choice": "Continue" });
    await consumeResolution(state, staleRequestID, { "stale-choice": "Continue" });

    const replacementRequestID = `${runID}-replacement`;
    const replacementQuestion = "Choose the fresh replacement answer.";
    await appendUserInput(state, replacementRequestID, [
      {
        id: "replacement-choice",
        header: "Fresh request",
        question: replacementQuestion,
        options: [
          { label: "Continue", description: "Complete the replacement request." },
          { label: "Stop", description: "Leave the replacement request pending." },
        ],
        multiSelect: false,
      },
    ]);
    await Promise.all([
      waitForQuestion(pageA, replacementQuestion),
      waitForQuestion(pageB, replacementQuestion),
    ]);
    await delay(450);
    const replacementPending = await listPending(productAPI, state.session.id);
    assert.equal(
      replacementPending.items.filter((item) => item.requestId === replacementRequestID).length,
      1,
    );
    assert.equal(
      await composer(pageB).getByRole("button", { name: /Stop/u }).locator("svg").count(),
      0,
      "the replacement request inherited the obsolete selection draft",
    );
    const replacementPath = resolvePath(state, replacementRequestID);
    const replacementResponsePromise = pageA.waitForResponse(
      (response) => responseMatches(response, replacementPath),
      { timeout: browserWaitMilliseconds },
    );
    await composer(pageA)
      .getByRole("button", { name: /Continue/u })
      .click();
    const replacementResponse = await replacementResponsePromise;
    assert.equal(replacementResponse.status(), 200);
    await Promise.all([
      waitForQuestionGone(pageA, replacementQuestion),
      waitForQuestionGone(pageB, replacementQuestion),
    ]);
    const replacementInteraction = await waitForInteraction(
      productAPI,
      state,
      replacementRequestID,
      "resolved",
    );
    assert.deepEqual(replacementInteraction.resolution.answers, {
      "replacement-choice": "Continue",
    });
    await consumeResolution(state, replacementRequestID, {
      "replacement-choice": "Continue",
    });

    const terminal = await completeExecution(
      productAPI,
      state,
      leaseKeeper,
      "Structured User Input converged across both live pages.",
    );
    await eventually("all interactions acknowledged", async () => {
      const interactions = await listInteractions(productAPI, state.execution.id);
      return interactions.length === 3 &&
        interactions.every(
          (interaction) =>
            interaction.status === "resolved" && interaction.deliveryStatus === "acknowledged",
        )
        ? interactions
        : false;
    });
    await pageA
      .getByText("Structured User Input converged across both live pages.", { exact: true })
      .waitFor({
        state: "visible",
        timeout: browserWaitMilliseconds,
      });
    await pageB
      .getByText("Structured User Input converged across both live pages.", { exact: true })
      .waitFor({
        state: "visible",
        timeout: browserWaitMilliseconds,
      });
    await Promise.all([
      assertSessionStreamBannerHidden(pageA),
      assertSessionStreamBannerHidden(pageB),
    ]);
    const screenshotA = `${evidenceDirectory}/page-a-terminal.png`;
    const screenshotB = `${evidenceDirectory}/page-b-terminal.png`;
    await pageA.screenshot({ path: screenshotA, fullPage: false });
    await pageB.screenshot({ path: screenshotB, fullPage: false });
    assertConsoleHealth(consoleIssues);

    return {
      mode: "automated",
      sessionUrl: `${webOrigin}/${state.session.id}`,
      tenantId: state.tenantID,
      projectId: state.project.id,
      sessionId: state.session.id,
      turnId: state.turn.id,
      executionId: state.execution.id,
      workerId: state.worker.id,
      generation: state.lease.generation,
      race: {
        statuses: raceStatuses,
        conflictCodes: raceProblemCodes,
        authoritativeAnswer: raceAnswer,
      },
      staleTimer: { obsoletePageRequests: stalePageBRequestCount, authoritativeAnswer: "Continue" },
      replacement: { authoritativeAnswer: "Continue", inheritedDraft: false },
      terminalEvents: terminal.terminalEvents.map((event) => event.eventType),
      lastSequence: terminal.page.lastSequence,
      sessionStreamBanners: { pageA: "hidden", pageB: "hidden" },
      consoleIssues,
      screenshots: [screenshotA, screenshotB],
    };
  } finally {
    await Promise.allSettled([
      pageAState.context.close(),
      pageBState.context.close(),
      browser.close(),
    ]);
  }
}

async function runManual(productAPI, state, leaseKeeper) {
  const requestID = `${runID}-manual`;
  const question = "Choose the manual browser acceptance answer.";
  await appendUserInput(state, requestID, [
    {
      id: "manual-choice",
      header: "Manual browser",
      question,
      options: [
        { label: "Continue", description: "Resolve the live browser acceptance request." },
        { label: "Stop", description: "Leave the acceptance request unresolved." },
      ],
      multiSelect: false,
    },
  ]);
  await eventually("manual pending snapshot", async () => {
    const pending = await listPending(productAPI, state.session.id);
    return pending.items.some((item) => item.requestId === requestID);
  });
  console.log(
    JSON.stringify({
      type: "manual-ready",
      sessionUrl: `${webOrigin}/${state.session.id}`,
      email: userEmail,
      displayName,
      question,
      expectedAnswer: "Continue",
    }),
  );

  const interaction = await eventually(
    "manual browser resolution",
    async () => {
      leaseKeeper.check();
      const interactions = await listInteractions(productAPI, state.execution.id);
      const item = interactions.find((candidate) => candidate.requestId === requestID);
      return item?.status === "resolved" ? item : false;
    },
    manualWaitMilliseconds,
  );
  assert.deepEqual(interaction.resolution.answers, { "manual-choice": "Continue" });
  await consumeResolution(state, requestID, { "manual-choice": "Continue" });
  const terminal = await completeExecution(
    productAPI,
    state,
    leaseKeeper,
    "Manual in-app browser Structured User Input acceptance completed.",
  );
  return {
    mode: "manual",
    sessionUrl: `${webOrigin}/${state.session.id}`,
    tenantId: state.tenantID,
    projectId: state.project.id,
    sessionId: state.session.id,
    turnId: state.turn.id,
    executionId: state.execution.id,
    workerId: state.worker.id,
    generation: state.lease.generation,
    answer: interaction.resolution.answers,
    deliveryStatus: "acknowledged",
    terminalEvents: terminal.terminalEvents.map((event) => event.eventType),
    lastSequence: terminal.page.lastSequence,
  };
}

async function main() {
  const productAPI = createProductAPI();
  let leaseKeeper;
  try {
    const state = await createLiveExecution(productAPI);
    leaseKeeper = startLeaseKeeper(state);
    const result = manualMode
      ? await runManual(productAPI, state, leaseKeeper)
      : await runAutomated(productAPI, state, leaseKeeper);
    console.log(JSON.stringify({ ok: true, ...result }, null, 2));
  } finally {
    if (leaseKeeper) {
      await leaseKeeper.stop().catch(() => {});
    }
    await productAPI.dispose();
  }
}

await main();
