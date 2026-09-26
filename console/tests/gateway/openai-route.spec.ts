import { readFileSync } from 'node:fs';
import { expect, test, type APIRequestContext, type Page } from '../playwright';
import { takeSecret, waitForRoutePublication } from '../journeys/fixtures';
import { verifyDraftSave } from '../journeys/draft-saving';
import { vertical } from '../journeys/fixtures';
import { verifyProviderRouting } from '../journeys/provider-routing';

// The owner created by tests/access/control.spec.ts; a run that starts on an
// empty installation performs the setup itself.
const owner = {
  email: 'owner@example.com',
  password: 'a long browser test password'
};
const upstream = {
  origin: 'http://127.0.0.1:4187',
  endpoint: 'http://127.0.0.1:4187/v1',
  model: 'compatible-e2e-model',
  credential: 'compatible-provider-secret',
  reply: 'Hello from the compatible upstream'
};
const route = 'compatible-chat';
const keyName = 'Compatible route key';

type ManagementResult = { status: number; body: Record<string, unknown> };

async function signIn(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (/\/setup$/.test(page.url())) {
    await page.getByLabel('Display name').fill('Owner');
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
    await page.getByRole('button', { name: 'Create owner account' }).click();
  } else {
    const deadline = Date.now() + 75_000;
    while (true) {
      await page.getByLabel('Email').fill(owner.email);
      await page.getByLabel('Password').fill(owner.password);
      const completed = page.waitForResponse(
        (response) =>
          response.request().method() === 'POST' &&
          new URL(response.url()).pathname === '/api/v1/sessions'
      );
      await page.getByRole('button', { name: 'Sign in' }).click();
      const response = await completed;
      if (response.status() !== 429 || Date.now() >= deadline) {
        expect(response.status()).toBe(201);
        break;
      }
      const seconds = Number(response.headers()['retry-after'] ?? '1');
      await new Promise((resolve) =>
        setTimeout(resolve, Math.min(60, Math.max(1, seconds)) * 1000)
      );
    }
  }
  await expect(page).toHaveURL(/\/$/);
}

/// Runs a management mutation from the signed-in page with the CSRF token and
/// the resource's current ETag, the way the console itself does.
async function manage(
  page: Page,
  method: 'POST' | 'PUT' | 'PATCH',
  path: string,
  options: { body?: unknown; match?: string; idempotency?: string } = {}
): Promise<ManagementResult> {
  return page.evaluate(
    async ({ method, path, options }) => {
      const session = await fetch('/api/v1/sessions/current').then((r) =>
        r.json()
      );
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token
      };
      if (options.match) {
        const current = await fetch(options.match).then((r) => r.json());
        headers['If-Match'] = `"${current.etag}"`;
      }
      if (options.idempotency) headers['Idempotency-Key'] = options.idempotency;
      const response = await fetch(path, {
        method,
        headers,
        body:
          options.body === undefined ? undefined : JSON.stringify(options.body)
      });
      return { status: response.status, body: await response.json() };
    },
    { method, path, options }
  );
}

async function gatewayStatus(page: Page, secret: string): Promise<number> {
  return page.evaluate(
    async ({ apiKey, slug }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${apiKey}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          messages: [{ role: 'user', content: 'Check route readiness.' }]
        })
      });
      await response.arrayBuffer();
      return response.status;
    },
    { apiKey: secret, slug: route }
  );
}

/// Streams one chat completion through the gateway and returns the assembled
/// text plus whether the stream terminated with [DONE].
function streamChat(page: Page, secret: string) {
  return page.evaluate(
    async ({ apiKey, slug }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${apiKey}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          stream: true,
          messages: [{ role: 'user', content: 'Stream a greeting.' }]
        })
      });
      const raw = await response.text();
      let text = '';
      let done = false;
      for (const line of raw.split('\n')) {
        if (!line.startsWith('data: ')) continue;
        const data = line.slice(6);
        if (data === '[DONE]') {
          done = true;
          continue;
        }
        const chunk = JSON.parse(data) as {
          choices?: Array<{ delta?: { content?: string } }>;
        };
        text += chunk.choices?.[0]?.delta?.content ?? '';
      }
      return {
        status: response.status,
        contentType: response.headers.get('content-type') ?? '',
        text,
        done
      };
    },
    { apiKey: secret, slug: route }
  );
}

async function resetUpstream(request: APIRequestContext, delayMs = 25) {
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);
  expect(
    (
      await request.post(`${upstream.origin}/__test__/delay`, {
        data: { ms: delayMs }
      })
    ).status()
  ).toBe(204);
}

async function upstreamRequests(request: APIRequestContext) {
  const response = await request.get(`${upstream.origin}/__test__/requests`);
  expect(response.ok()).toBe(true);
  return (await response.json()) as {
    requests: Array<{
      path: string;
      headers: Record<string, string>;
      body: unknown;
    }>;
    unexpected: string[];
  };
}

async function assertUpstreamCredentials(
  request: APIRequestContext,
  secret: string
) {
  const observed = await upstreamRequests(request);
  expect(observed.unexpected).toEqual([]);
  expect(observed.requests.length).toBeGreaterThan(0);
  for (const call of observed.requests) {
    expect(call.headers.authorization).toBe(`Bearer ${upstream.credential}`);
  }
  expect(JSON.stringify(observed.requests)).not.toContain(secret);
}

test('a browser user configures an OpenAI-compatible route and reaches unary and streaming inference', async ({
  page,
  request
}, info) => {
  test.setTimeout(180_000);
  await signIn(page);
  await resetUpstream(request);

  // Provider wizard: connect, discover, certify, activate.
  await page.goto('/providers/new');
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
  await page.getByLabel('Provider name').fill('Compatible upstream');
  await page.getByLabel('Authentication').selectOption('api_key');
  await page
    .getByRole('textbox', { name: 'Endpoint', exact: true })
    .fill(upstream.endpoint);
  await page.getByLabel('Seed model (optional)').fill(upstream.model);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(upstream.credential);
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/api/v1/providers'
  );
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const providerId = ((await (await created).json()) as { id: string }).id;
  await expect(page.getByText(upstream.credential)).toHaveCount(0);
  await expect(
    page.getByRole('heading', { name: 'Discover upstream models' })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(
    page.getByRole('heading', { name: 'Review model capabilities' })
  ).toBeVisible();
  await expect(
    page.getByText(upstream.model, { exact: true }).first()
  ).toBeVisible();
  // The seed model arrives with both supported tuples declared, so the review
  // confirms them; auxiliary operations remain available for an explicit review.
  await expect(page.getByLabel(/Operation \d+/)).toHaveCount(2);
  await expect(
    page.getByRole('button', { name: 'Add capability' })
  ).toBeEnabled();
  await page.getByLabel('Operation 1').selectOption('generation');
  await page.getByLabel('Client surface 1').selectOption('openai');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByLabel('Operation 2').selectOption('generation');
  await page.getByLabel('Client surface 2').selectOption('openai');
  await page.getByLabel('Mode 2').selectOption('streaming');
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText(/reviewed tuples passed server certification/)
  ).toBeVisible();
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({
    path: info.outputPath('provider-onboarding.png'),
    fullPage: true
  });
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();

  // Route draft: stale edits stay local, inactive providers block activation.
  await page.getByRole('link', { name: 'Build default route' }).click();
  await expect(
    page.getByRole('heading', { name: 'Build a route draft.' })
  ).toBeVisible();
  await page.getByLabel('Public model slug').fill(route);
  await page.getByRole('button', { name: 'Add target' }).click();
  await expect(
    page.getByLabel('Provider model').first().locator('option:checked')
  ).toContainText(upstream.model);
  await page.getByLabel('Maximum attempts').fill('2');
  // This provider has no profile, so the route is declared transformed.
  await page.getByLabel('Fidelity mode').selectOption('transformed');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);
  await verifyDraftSave(page, 'route', route);

  const providerPath = `/api/v1/providers/${providerId}`;
  const disabled = await manage(page, 'POST', `${providerPath}/disable`, {
    match: providerPath,
    idempotency: `disable-${Date.now()}`
  });
  expect(disabled.status).toBe(200);
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(
    page.getByText(/belongs to a connection that is not active\./)
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath('route-activation-blocked.png'),
    fullPage: true
  });
  const reactivated = await manage(page, 'POST', `${providerPath}/activate`, {
    match: providerPath,
    idempotency: `activate-${Date.now()}`
  });
  expect(reactivated.status).toBe(200);
  await page.getByLabel('Dry-run operation').selectOption('generation');
  await page.getByLabel('Client surface').selectOption('openai');
  await page.getByLabel('Transport mode').selectOption('streaming');
  await page.getByRole('button', { name: 'Simulate order' }).click();
  await expect(
    page.getByRole('heading', { name: 'Attempt explanation' })
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
  await page.screenshot({
    path: info.outputPath('route-active.png'),
    fullPage: true
  });

  // Key issuance with an explicit route allowlist, then real SDK-shaped traffic.
  await page.goto('/api-keys/new');
  await expect(
    page.getByRole('heading', { name: 'Create a proxy key.' })
  ).toBeVisible();
  await page.getByLabel('Key name').fill(keyName);
  await expect(
    page.getByRole('checkbox', { name: 'Inference requests' })
  ).toBeChecked();
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  await page
    .getByRole('group', { name: 'Allowed route slugs' })
    .getByRole('checkbox', { name: route })
    .check();
  await page.getByRole('button', { name: /Create and show key/ }).click();
  const secretDialog = page.getByRole('dialog', {
    name: 'Copy this secret now.'
  });
  await expect(secretDialog).toBeVisible();
  const secret = await takeSecret(secretDialog);
  expect(secret).toMatch(/^olp_/);
  await waitForRoutePublication(page, secret, route);
  await resetUpstream(request);
  await secretDialog.getByRole('tab', { name: 'OpenAI Python' }).click();
  const connectionTest = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/v1/responses'
  );
  await secretDialog
    .getByRole('button', { name: 'Run connection test' })
    .click();
  expect((await connectionTest).status()).toBe(200);
  await expect(
    secretDialog.getByText(`OpenAI request succeeded through route ${route}.`)
  ).toBeVisible();
  await secretDialog.screenshot({
    path: info.outputPath('key-connection-test.png')
  });
  await secretDialog
    .getByRole('button', { name: 'I have saved the key' })
    .click();
  await expect(secretDialog).toBeHidden();

  await assertUpstreamCredentials(request, secret);

  // A stream admitted before a publication change keeps its pinned snapshot.
  await resetUpstream(request);
  expect(
    (await request.post(`${upstream.origin}/__test__/hold`)).status()
  ).toBe(204);
  const streaming = streamChat(page, secret);
  try {
    await expect
      .poll(async () => (await upstreamRequests(request)).requests.length, {
        timeout: 10_000
      })
      .toBeGreaterThan(0);
    expect(
      (
        await manage(page, 'POST', `${providerPath}/disable`, {
          match: providerPath,
          idempotency: `disable-inflight-${Date.now()}`
        })
      ).status
    ).toBe(200);
    // Wait for the gateway to install the disabled release, not just for
    // the management transaction to publish it.
    await expect
      .poll(() => gatewayStatus(page, secret), { timeout: 15_000 })
      .toBe(503);
  } finally {
    expect(
      (await request.post(`${upstream.origin}/__test__/resume`)).status()
    ).toBe(204);
    await streaming.catch(() => undefined);
  }
  const streamed = await streaming;
  expect(streamed.status).toBe(200);
  expect(streamed.contentType).toContain('text/event-stream');
  expect(streamed.text).toBe(upstream.reply);
  expect(streamed.done).toBe(true);
  expect(
    (
      await manage(page, 'POST', `${providerPath}/activate`, {
        match: providerPath,
        idempotency: `activate-inflight-${Date.now()}`
      })
    ).status
  ).toBe(200);
  await expect
    .poll(() => gatewayStatus(page, secret), { timeout: 15_000 })
    .toBe(200);

  await assertUpstreamCredentials(request, secret);

  // The console playground uses the same runtime through the session.
  await resetUpstream(request);
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(route);
  await page.getByLabel('Prompt').fill('Say hello.');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.locator('.output pre').first()).toHaveText(upstream.reply);
  await page.screenshot({
    path: info.outputPath('playground.png'),
    fullPage: true
  });

  // Only the configured provider credential ever reached the upstream.
  await assertUpstreamCredentials(request, secret);

  // Revocation is authority state: the key stops working within the poll.
  await page.goto('/api-keys');
  const row = page.getByRole('row').filter({ hasText: keyName });
  await expect(row).toHaveCount(1);
  page.once('dialog', (dialog) => dialog.accept());
  await row.getByRole('button', { name: 'Revoke' }).click();
  await expect(row.getByText('revoked', { exact: true })).toBeVisible();
  await expect
    .poll(() => gatewayStatus(page, secret), {
      timeout: 15_000,
      intervals: [250, 500, 1_000]
    })
    .toBe(401);

  // A capability lookup failure is not evidence that history is disabled.
  const historyRequests: string[] = [];
  page.on('request', (outgoing) => {
    const path = new URL(outgoing.url()).pathname;
    if (path.startsWith('/api/v1/requests')) historyRequests.push(path);
  });
  const capabilitiesPattern = '**/api/v1/auth/capabilities';
  await page.route(capabilitiesPattern, (intercept) =>
    intercept.fulfill({
      status: 503,
      contentType: 'application/problem+json',
      headers: { 'cache-control': 'no-store' },
      body: JSON.stringify({ title: 'Unavailable', status: 503 })
    })
  );
  try {
    await page.goto('/requests');
    await expect(
      page.getByText('Request history capabilities are unavailable.')
    ).toBeVisible({ timeout: 15_000 });
    expect(historyRequests).toEqual([]);
  } finally {
    await page.unroute(capabilitiesPattern);
  }
  // Once the capability answer arrives, retention is enforced here, so the
  // explorer queries retained metadata instead of disclaiming it.
  const listed = page.waitForResponse(
    (response) =>
      response.request().method() === 'GET' &&
      new URL(response.url()).pathname === '/api/v1/requests'
  );
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  expect((await listed).status()).toBe(200);
  await expect(
    page.getByRole('button', { name: 'Apply filters' })
  ).toBeVisible();
  await expect(
    page.getByText(/Request history is not retained by this installation/)
  ).toHaveCount(0);
});

test('cloud connections support bulk model review, credential pools and policy routing', async ({
  page
}, info) => {
  test.setTimeout(240_000);
  await signIn(page);
  await page.goto('/providers/new');
  await page.getByRole('radio', { name: /Azure OpenAI/ }).check();
  await page.getByLabel('Provider name').fill('Concurrent provider name');
  await page.getByLabel('Seed model (optional)').fill(vertical.deployment);
  await page.getByLabel('Azure resource endpoint').fill(vertical.endpoint);
  await page.getByLabel('API version').fill(vertical.apiVersion);
  await page.getByLabel('Cloud deployment').fill(vertical.deployment);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(vertical.credential);
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const bulk = page.locator('section').filter({
    has: page.getByRole('heading', { name: 'Validate models in bulk' })
  });
  await bulk.getByRole('checkbox').check();
  await bulk.getByLabel('Capabilities to validate').selectOption('generation');
  await bulk
    .getByRole('button', { name: 'Validate 1 selected models' })
    .click();
  await expect(bulk.getByText(/Checked 1 models/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();
  await verifyProviderRouting(page, info);
});
