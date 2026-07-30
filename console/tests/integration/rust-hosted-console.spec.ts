import AxeBuilder from '@axe-core/playwright';
import {
  expect,
  test,
  type APIRequestContext,
  type Locator,
  type Page
} from '../playwright';

const owner = {
  name: 'Integration Owner',
  email: 'console-integration@example.com',
  password: 'correct horse battery staple'
};
// Test-only fixed material. The application still loads it from the mounted
// secret file, while the browser uses the same base64 token through the setup
// field without requiring Node globals in the Svelte typecheck.
const bootstrapToken = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=';

const vertical = {
  providerName: 'Vertical Azure provider',
  deployment: 'vertical-e2e-deployment',
  endpoint: 'http://127.0.0.1:4178',
  apiVersion: '2024-10-21',
  credential: 'vertical-provider-secret',
  route: 'vertical-all-protocols',
  keyName: 'Vertical all-protocol key',
  reply: 'Hello from the vertical upstream'
};

type ClientSurface = 'openai' | 'anthropic' | 'gemini';

type ObservedGatewayResponse = {
  path: string;
  status: number;
  body: unknown;
};

type RecordedUpstreamRequest = {
  method: string;
  path: string;
  query: string;
  headers: Record<string, string | string[] | undefined>;
  body: unknown;
};

type UpstreamSnapshot = {
  requests: RecordedUpstreamRequest[];
  unexpected: string[];
};

async function signInAsOwner(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password').fill(owner.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/$/);
}

/// Reads the one-time secret out of the reveal dialog.
async function takeSecret(dialog: Locator): Promise<string> {
  const secret = (await dialog.locator('.secret-value, code, pre').first().textContent())?.trim();
  return secret ?? '';
}

/// Asserts a secret is nowhere in the rendered page.
///
/// The search runs in the page and only the verdict crosses the wire, so a
/// failure reports the claim instead of dumping the entire document into the
/// run log.
async function expectSecretGone(page: Page, secret: string, what: string): Promise<void> {
  const present = await page.evaluate(
    (needle) => document.documentElement.outerHTML.includes(needle),
    secret
  );
  expect(present, `${what} must not be retrievable after it is dismissed`).toBe(false);
}

async function resetUpstream(request: APIRequestContext): Promise<void> {
  const response = await request.post('http://127.0.0.1:4178/__test__/reset');
  expect(response.status()).toBe(204);
}

async function upstreamSnapshot(request: APIRequestContext): Promise<UpstreamSnapshot> {
  const response = await request.get('http://127.0.0.1:4178/__test__/requests');
  expect(response.ok()).toBe(true);
  return (await response.json()) as UpstreamSnapshot;
}

async function waitForRoutePublication(
  page: Page,
  secret: string,
  route: string
): Promise<void> {
  await expect
    .poll(
      async () =>
        page.evaluate(
          async ({ apiKey, routeSlug }) => {
            try {
              const response = await fetch('/openai/v1/models', {
                headers: { Authorization: `Bearer ${apiKey}` }
              });
              if (!response.ok) {
                await response.body?.cancel();
                return false;
              }
              const payload = (await response.json()) as { data?: Array<{ id?: string }> };
              return payload.data?.some((model) => model.id === routeSlug) ?? false;
            } catch {
              return false;
            }
          },
          { apiKey: secret, routeSlug: route }
        ),
      {
        message: `route ${route} and its API key should publish to the runtime`,
        timeout: 30_000,
        intervals: [250, 500, 1_000]
      }
    )
    .toBe(true);
}

async function installGatewayResponseObserver(page: Page): Promise<void> {
  await page.evaluate(() => {
    type ObservedWindow = Window & {
      __olpObservedGatewayResponses?: ObservedGatewayResponse[];
    };

    const observedWindow = window as ObservedWindow;
    observedWindow.__olpObservedGatewayResponses = [];
    const nativeFetch = window.fetch.bind(window);
    window.fetch = async (...args: Parameters<typeof window.fetch>): Promise<Response> => {
      const response = await nativeFetch(...args);
      const input = args[0];
      const rawUrl =
        typeof input === 'string' || input instanceof URL ? input.toString() : input.url;
      const path = new URL(rawUrl, window.location.origin).pathname;
      const isGatewayRequest =
        path === '/openai/v1/responses' ||
        path === '/anthropic/v1/messages' ||
        path.startsWith('/gemini/v1beta/models/');
      if (isGatewayRequest) {
        let body: unknown = null;
        try {
          body = await response.clone().json();
        } catch {
          // A non-JSON body is recorded as null and fails the protocol assertion.
        }
        observedWindow.__olpObservedGatewayResponses?.push({
          path,
          status: response.status,
          body
        });
      }
      return response;
    };
  });
}

async function observedGatewayResponses(page: Page): Promise<ObservedGatewayResponse[]> {
  return page.evaluate(() => {
    type ObservedWindow = Window & {
      __olpObservedGatewayResponses?: ObservedGatewayResponse[];
    };
    return (window as ObservedWindow).__olpObservedGatewayResponses ?? [];
  });
}

async function refreshUntilRequestCount(page: Page, count: number): Promise<void> {
  const rows = page.locator('table tbody tr');
  await expect
    .poll(
      async () => {
        const current = await rows.count();
        if (current === count) return current;

        const response = page.waitForResponse(
          (candidate) =>
            candidate.request().method() === 'GET' &&
            new URL(candidate.url()).pathname === '/api/v1/requests'
        );
        await page.getByRole('button', { name: 'Refresh' }).click();
        await response;
        return rows.count();
      },
      {
        message: `${count} gateway requests should persist exactly once`,
        timeout: 30_000,
        intervals: [250, 500, 1_000]
      }
    )
    .toBe(count);
}

async function expectFact(page: Page, label: string, value: string): Promise<void> {
  const term = page.locator('dt').filter({ hasText: new RegExp(`^${label}$`) });
  await expect(term).toHaveCount(1);
  await expect(term.locator('xpath=following-sibling::dd[1]')).toHaveText(value);
}

test.describe('Rust-hosted console integration', () => {
  test.describe.configure({ mode: 'serial' });

test('Rust serves the console and enforces the real setup/session/management boundary', async ({ page, context }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill(owner.name);
  await page.getByLabel('Work email').fill(owner.email);
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByLabel('Confirm password').fill(owner.password);
  await page.getByLabel('Setup token').fill(bootstrapToken);
  await page.getByRole('button', { name: 'Create owner account' }).click();

  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole('heading', { name: 'Bring your first model route online.' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByRole('link', { name: 'Providers' }).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No providers configured' })).toBeVisible();

  await page.getByRole('link', { name: 'Access', exact: true }).click();
  await page.getByRole('link', { name: 'Invite member' }).click();
  await page.getByLabel('Email address').fill('invited-integration@example.com');
  await page.getByLabel('Role').selectOption('developer');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitationDialog = page.getByRole('dialog', { name: 'Copy the invitation link now.' });
  const invitationToken = (await invitationDialog.locator('.invitation-token').textContent())?.trim();
  expect(invitationToken).toBeTruthy();
  await invitationDialog.getByRole('button', { name: 'I have shared it' }).click();

  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Expected issuer').fill('http://127.0.0.1:4176');
  await page
    .getByLabel('Discovery URL')
    .fill('http://127.0.0.1:4176/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('console-browser-client');
  await page.getByLabel('Client secret').fill('write-only-browser-secret');
  await page.getByLabel('Enabled').check();
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(page.getByText('OIDC configuration validated and enabled.')).toBeVisible();

  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByRole('link', { name: 'Continue with single sign-on' }).click();
  await expect(page).toHaveURL(/^http:\/\/127\.0\.0\.1:4176\/authorize\?/);
  const oidcCookies = (await context.cookies('http://localhost:4175')).filter((cookie) =>
    cookie.name.startsWith('__Host-olp_oidc_')
  );
  expect(oidcCookies).toHaveLength(1);
  expect(oidcCookies[0]?.name).toMatch(
    /^__Host-olp_oidc_login_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
  );
  for (const cookie of oidcCookies) {
    expect(cookie.domain).toBe('localhost');
    expect(cookie.path).toBe('/');
    expect(cookie.secure).toBe(true);
    expect(cookie.httpOnly).toBe(true);
    expect(cookie.sameSite).toBe('Lax');
  }

  await page.goto('/providers');
  await expect(page).toHaveURL(/\/login\?return_to=%2Fproviders$/);
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();

  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password').fill(owner.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await page.goto(`/invitations/accept#token=${encodeURIComponent(invitationToken!)}`);
  await expect(page).toHaveURL(/\/invitations\/accept$/);
  await page.getByLabel('Display name').fill('Invited Integration User');
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByLabel('Confirm password').fill(owner.password);
  await page.getByRole('button', { name: 'Accept invitation' }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByText('Invited Integration User')).toBeVisible();
});

test('browser configures one route, crosses all client protocols, and reads persisted telemetry', async ({
  page,
  request
}) => {
  test.setTimeout(120_000);
  await signInAsOwner(page);

  await page.goto('/providers/new');
  await expect(page.getByRole('heading', { name: 'Connect an upstream provider.' })).toBeVisible();
  await page.getByRole('radio', { name: /Azure OpenAI/ }).check();
  await page.getByLabel('Provider name').fill(vertical.providerName);
  await page.getByLabel('Authentication').selectOption('api_key');
  await page.getByLabel('Seed model (optional)').fill(vertical.deployment);
  await page.getByLabel('Azure resource endpoint').fill(vertical.endpoint);
  await page.getByLabel('API version').fill(vertical.apiVersion);
  await page.getByLabel('Cloud deployment').fill(vertical.deployment);
  await page.getByLabel('Credential', { exact: true }).fill(vertical.credential);
  await page.getByRole('button', { name: /Save and test connection/ }).click();

  await expect(page.getByRole('heading', { name: 'Verify upstream reachability' })).toBeVisible();
  await expect(page.getByText(vertical.credential)).toHaveCount(0);
  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.getByRole('heading', { name: 'Discover upstream models' })).toBeVisible();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(page.getByRole('heading', { name: 'Review model capabilities' })).toBeVisible();
  await expect(page.getByText(vertical.deployment, { exact: true }).first()).toBeVisible();

  for (let index = 0; index < 3; index += 1) {
    await page.getByRole('button', { name: 'Add capability' }).click();
  }
  const surfaces: ClientSurface[] = ['openai', 'anthropic', 'gemini'];
  for (const [offset, surface] of surfaces.entries()) {
    const index = offset + 1;
    await page.getByLabel(`Operation ${index}`).selectOption('generation');
    await page.getByLabel(`Client surface ${index}`).selectOption(surface);
    await page.getByLabel(`Mode ${index}`).selectOption('unary');
  }
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(page.getByText('Capability review saved with declared provenance.')).toBeVisible();
  await page.getByRole('button', { name: 'Server-certify capabilities' }).click();
  await expect(page.getByText(/reviewed tuples passed server certification/)).toBeVisible();
  await expect(page.getByText('3/3 certified', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate provider' })).toBeEnabled();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(page.getByRole('heading', { name: 'Now build a stable route slug.' })).toBeVisible();

  await page.getByRole('link', { name: 'Build default route' }).click();
  await expect(page.getByRole('heading', { name: 'Build a route draft.' })).toBeVisible();
  await page.getByLabel('Public model slug').fill(vertical.route);
  await page.getByRole('button', { name: 'Add target' }).click();
  await expect(
    page.getByLabel('Provider model').first().locator('option:checked')
  ).toContainText(vertical.deployment);
  await page.getByLabel('Maximum attempts').fill('1');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);

  await page.getByLabel('Dry-run operation').selectOption('generation');
  for (const surface of surfaces) {
    await page.getByLabel('Client surface').selectOption(surface);
    await page.getByLabel('Transport mode').selectOption('unary');
    await page.getByRole('button', { name: 'Simulate order' }).click();
    await expect(page.getByRole('heading', { name: 'Attempt explanation' })).toBeVisible();
    await expect(page.getByText('Eligible in priority group 1')).toBeVisible();
  }
  await page.getByRole('button', { name: 'Validate draft' }).click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page.getByRole('button', { name: 'Activate route' }).click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();

  await page.goto('/api-keys/new');
  await expect(page.getByRole('heading', { name: 'Create a proxy key.' })).toBeVisible();
  await page.getByLabel('Key name').fill(vertical.keyName);
  await expect(page.getByRole('checkbox', { name: 'Inference requests' })).toBeChecked();
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  await page
    .getByRole('group', { name: 'Allowed route slugs' })
    .getByRole('checkbox', { name: vertical.route })
    .check();
  await page.getByRole('button', { name: /Create and show key/ }).click();

  const secretDialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(secretDialog).toBeVisible();
  const secret = await takeSecret(secretDialog);
  expect(secret).toMatch(/^olp_/);
  await waitForRoutePublication(page, secret, vertical.route);
  await resetUpstream(request);
  await installGatewayResponseObserver(page);

  const protocolCases = [
    {
      tab: 'OpenAI Python',
      vendor: 'OpenAI',
      path: '/openai/v1/responses'
    },
    {
      tab: 'Anthropic TS',
      vendor: 'Anthropic',
      path: '/anthropic/v1/messages'
    },
    {
      tab: 'Gemini TS',
      vendor: 'Gemini',
      path: `/gemini/v1beta/models/${vertical.route}:generateContent`
    }
  ] as const;
  for (const protocol of protocolCases) {
    await secretDialog.getByRole('tab', { name: protocol.tab }).click();
    const gatewayResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === protocol.path
    );
    await secretDialog.getByRole('button', { name: 'Run connection test' }).click();
    expect((await gatewayResponse).status()).toBe(200);
    await expect(
      secretDialog.getByText(
        `${protocol.vendor} request succeeded through route ${vertical.route}.`
      )
    ).toBeVisible();
  }

  const observed = await observedGatewayResponses(page);
  expect(observed.map(({ path }) => path)).toEqual(protocolCases.map(({ path }) => path));
  expect(observed.map(({ status }) => status)).toEqual([200, 200, 200]);
  expect(observed[0]?.body).toMatchObject({
    object: 'response',
    status: 'completed',
    output: [
      {
        type: 'message',
        role: 'assistant',
        content: [{ type: 'output_text', text: vertical.reply }]
      }
    ],
    usage: { input_tokens: 7, output_tokens: 5, total_tokens: 12 }
  });
  expect(observed[1]?.body).toMatchObject({
    type: 'message',
    role: 'assistant',
    content: [{ type: 'text', text: vertical.reply }],
    stop_reason: 'end_turn',
    usage: { input_tokens: 7, output_tokens: 5 }
  });
  expect(observed[2]?.body).toMatchObject({
    candidates: [
      {
        content: { role: 'model', parts: [{ text: vertical.reply }] },
        finishReason: 'STOP'
      }
    ],
    usageMetadata: {
      promptTokenCount: 7,
      candidatesTokenCount: 5,
      totalTokenCount: 12
    }
  });

  const upstream = await upstreamSnapshot(request);
  expect(upstream.unexpected).toEqual([]);
  expect(upstream.requests).toHaveLength(3);
  expect(upstream.requests.map(({ path }) => path)).toEqual([
    `/openai/deployments/${vertical.deployment}/responses`,
    `/openai/deployments/${vertical.deployment}/chat/completions`,
    `/openai/deployments/${vertical.deployment}/chat/completions`
  ]);
  for (const call of upstream.requests) {
    expect(call.method).toBe('POST');
    expect(new URLSearchParams(call.query).get('api-version')).toBe(vertical.apiVersion);
    expect(call.headers['api-key']).toBe(vertical.credential);
    expect(call.headers.authorization).toBeUndefined();
  }
  expect(upstream.requests[0]?.body).toMatchObject({
    model: vertical.deployment,
    input: [
      {
        type: 'message',
        role: 'user',
        content: [{ type: 'input_text', text: 'Connection test' }]
      }
    ],
    max_output_tokens: 16
  });
  for (const call of upstream.requests.slice(1)) {
    expect(call.body).toMatchObject({
      model: vertical.deployment,
      messages: [{ role: 'user', content: 'Connection test' }],
      max_completion_tokens: 16
    });
  }
  expect(
    JSON.stringify(upstream.requests).includes(secret),
    'the client API key must never reach the provider'
  ).toBe(false);

  await secretDialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(secretDialog).toBeHidden();
  await page.goto('/requests');
  await expect(page.getByRole('heading', { name: 'Request Explorer' })).toBeVisible();
  await page.getByLabel('Route', { exact: true }).fill(vertical.route);
  await page.getByLabel('Operation', { exact: true }).fill('generation');
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await refreshUntilRequestCount(page, 3);

  const requestLinks = new Map<ClientSurface, string>();
  for (const surface of surfaces) {
    const row = page
      .locator('.desktop-results tbody tr')
      .filter({ hasText: `generation · ${surface}` });
    await expect(row).toHaveCount(1);
    await expect(row.locator('td').nth(1)).toContainText(vertical.route);
    await expect(row.locator('td').nth(2)).toHaveText('200');
    await expect(row.locator('td').nth(3)).toHaveText('1');
    await expect(row.locator('td').nth(5)).toContainText('7 in');
    await expect(row.locator('td').nth(5)).toContainText('5 out');
    const href = await row.getByRole('link', { name: /^View request/ }).getAttribute('href');
    expect(href).toBeTruthy();
    requestLinks.set(surface, href!);
  }

  for (const surface of surfaces) {
    await page.goto(requestLinks.get(surface)!);
    await expect(page.getByRole('heading', { name: 'Request timeline' })).toBeVisible();
    await expect(page.getByRole('heading', { name: vertical.route })).toBeVisible();
    await expect(page.getByText('1 attempts', { exact: true })).toBeVisible();
    await expectFact(page, 'Operation', 'generation');
    await expectFact(page, 'Client surface', surface);
    await expectFact(page, 'Input tokens', '7');
    await expectFact(page, 'Output tokens', '5');
    await expectFact(page, 'Usage completeness', 'Complete');
    await expect(page.getByText(vertical.providerName, { exact: true })).toBeVisible();
    await expect(page.getByText(vertical.deployment, { exact: true })).toBeVisible();
    await expectFact(page, 'Response committed', 'Yes — failover stopped');
  }
});

test('API key secrets are shown once and the lifecycle converges against the real backend', async ({
  page,
  context
}) => {
  page.on('dialog', (dialog) => dialog.accept());

  await signInAsOwner(page);

  // The mocked specs install their own cookie, so only this tier can show that
  // the server issues the `__Host-` contract the console depends on.
  const session = (await context.cookies(new URL(page.url()).origin)).filter((cookie) =>
    cookie.name.startsWith('__Host-')
  );
  expect(session.length).toBeGreaterThan(0);
  for (const cookie of session) {
    expect(cookie.path).toBe('/');
    expect(cookie.secure).toBe(true);
  }

  const keyName = `integration key ${Date.now()}`;
  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill(keyName);
  await page.getByLabel('Requests per minute').fill('120');
  await page.getByLabel('Concurrent requests').fill('8');
  await page.getByRole('button', { name: /Create and show key/ }).click();

  const created = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(created).toBeVisible();
  const secret = await takeSecret(created);
  // `CreateApiKeyResponse.secret` is documented "Returned only by this creation
  // response", so the value has to be real and then has to disappear.
  expect(secret).toMatch(/^olp_/);
  expect((await new AxeBuilder({ page }).include('.secret-dialog').analyze()).violations).toEqual(
    []
  );
  await created.getByRole('button', { name: 'I have saved the key' }).click();

  await expect(page).toHaveURL(/\/api-keys$/);
  await expect(page.getByText(keyName)).toBeVisible();
  await expect(page.getByText(secret)).toHaveCount(0);

  // A reload goes back to the server: if the secret came back on a listing,
  // "returned only by this creation response" would be false.
  await page.reload();
  await expect(page.getByText(keyName)).toBeVisible();
  await expectSecretGone(page, secret, 'the created secret');

  const row = page.getByRole('row').filter({ hasText: keyName });
  await expect(row).toHaveCount(1);
  await row.getByRole('button', { name: 'Rotate' }).click();
  const rotated = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(rotated).toBeVisible();
  const rotatedSecret = await takeSecret(rotated);
  expect(rotatedSecret).toMatch(/^olp_/);
  // A rotation that returned the same material would not be a rotation.
  expect(rotatedSecret).not.toBe(secret);
  await rotated.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(rotated).toBeHidden();
  await page.reload();
  await expectSecretGone(page, rotatedSecret, 'the rotated secret');

  await row.getByRole('button', { name: 'Revoke' }).click();
  await expect(row.getByText('revoked', { exact: true })).toBeVisible();

  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
});
