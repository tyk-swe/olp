import {
  owner,
  vertical,
  signInAsOwner,
  takeSecret,
  waitForRoutePublication
} from './fixtures';
import { verifyConsolePolish } from './console-polish';
import { verifyDraftSave } from './draft-saving';
import { expectFact, refreshUntilRequestCount } from './request-history';
import { readFileSync } from 'node:fs';
import AxeBuilder from '@axe-core/playwright';
import { expect, test, type APIRequestContext, type Page } from '../playwright';

const bootstrapToken = readFileSync(
  process.env.OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE!,
  'utf8'
).trim();

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

/// Asserts a secret is nowhere in the rendered page.
///
/// The search runs in the page and only the verdict crosses the wire, so a
/// failure reports the claim instead of dumping the entire document into the
/// run log.
async function expectSecretGone(
  page: Page,
  secret: string,
  what: string
): Promise<void> {
  const present = await page.evaluate(
    (needle) => document.documentElement.outerHTML.includes(needle),
    secret
  );
  expect(present, `${what} must not be retrievable after it is dismissed`).toBe(
    false
  );
}

async function resetUpstream(request: APIRequestContext): Promise<void> {
  const response = await request.post('http://127.0.0.1:4178/__test__/reset');
  expect(response.status()).toBe(204);
}

async function upstreamSnapshot(
  request: APIRequestContext
): Promise<UpstreamSnapshot> {
  const response = await request.get('http://127.0.0.1:4178/__test__/requests');
  expect(response.ok()).toBe(true);
  return (await response.json()) as UpstreamSnapshot;
}

async function installGatewayResponseObserver(page: Page): Promise<void> {
  await page.evaluate(() => {
    type ObservedWindow = Window & {
      __olpObservedGatewayResponses?: ObservedGatewayResponse[];
    };

    const observedWindow = window as ObservedWindow;
    observedWindow.__olpObservedGatewayResponses = [];
    const nativeFetch = window.fetch.bind(window);
    window.fetch = async (
      ...args: Parameters<typeof window.fetch>
    ): Promise<Response> => {
      const response = await nativeFetch(...args);
      const input = args[0];
      const rawUrl =
        typeof input === 'string' || input instanceof URL
          ? input.toString()
          : input.url;
      const path = new URL(rawUrl, window.location.origin).pathname;
      const isGatewayRequest =
        path === '/v1/responses' ||
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

async function observedGatewayResponses(
  page: Page
): Promise<ObservedGatewayResponse[]> {
  return page.evaluate(() => {
    type ObservedWindow = Window & {
      __olpObservedGatewayResponses?: ObservedGatewayResponse[];
    };
    return (window as ObservedWindow).__olpObservedGatewayResponses ?? [];
  });
}

test.describe('Rust-hosted console integration', () => {
  test.describe.configure({ mode: 'serial' });
  test.skip(
    process.env.OLP_CONSOLE_E2E_RESTORED === 'true',
    'The restored database has already completed setup.'
  );

  test('Rust serves the console and enforces the real setup/session/management boundary', async ({
    page,
    context
  }) => {
    await page.goto('/');
    await expect(page).toHaveURL(/\/setup$/);
    await page.getByLabel('Display name').fill(owner.name);
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page.getByLabel('Setup token').fill(bootstrapToken);
    await page.getByRole('button', { name: 'Create owner account' }).click();

    await expect(page).toHaveURL(/\/$/);
    await expect(
      page.getByRole('heading', {
        name: 'Bring your first model route online.'
      })
    ).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

    await page.getByRole('link', { name: 'Providers' }).click();
    await expect(page).toHaveURL(/\/providers$/);
    await expect(
      page.getByRole('heading', { name: 'Providers', exact: true })
    ).toBeVisible();
    await expect(
      page.getByRole('heading', { name: 'No providers configured' })
    ).toBeVisible();

    await page.getByRole('link', { name: 'Access', exact: true }).click();
    await page.getByRole('button', { name: 'Invite member' }).click();
    await page
      .getByLabel('Email address')
      .fill('invited-integration@example.com');
    await page.getByLabel('Role').selectOption('developer');
    await page.getByRole('button', { name: 'Create invitation' }).click();
    const invitationDialog = page.getByRole('dialog', {
      name: 'Copy the invitation link now.'
    });
    const invitationLink = (
      await invitationDialog.locator('.invitation-token').textContent()
    )?.trim();
    const invitationToken = decodeURIComponent(
      new URL(invitationLink ?? '').hash.replace('#token=', '')
    );
    expect(invitationToken).toBeTruthy();
    await invitationDialog
      .getByRole('button', { name: 'I have shared it' })
      .click();

    const oidcJourney = process.env.OLP_CONSOLE_E2E_CANDIDATE !== 'true';
    if (oidcJourney) {
      await page.getByRole('button', { name: 'OIDC' }).click();
      await page.getByLabel('Expected issuer').fill('http://127.0.0.1:4176');
      await page
        .getByLabel('Discovery URL')
        .fill('http://127.0.0.1:4176/.well-known/openid-configuration');
      await page.getByLabel('Client ID').fill('console-browser-client');
      await page.getByLabel('Client secret').fill('write-only-browser-secret');
      await page.getByLabel('Enabled').check();
      await page.getByRole('button', { name: 'Save and validate' }).click();
      await expect(
        page.getByText('OIDC configuration validated and enabled.')
      ).toBeVisible();
    }

    await page.getByRole('button', { name: 'Open account menu' }).click();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/login$/);

    if (oidcJourney) {
      await page
        .getByRole('link', { name: 'Continue with single sign-on' })
        .click();
      await expect(page).toHaveURL(/^http:\/\/127\.0\.0\.1:4176\/authorize\?/);
      const oidcCookies = (
        await context.cookies('http://localhost:4175')
      ).filter((cookie) => cookie.name.startsWith('__Host-olp_oidc_'));
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
    }
    await page.goto('/providers');
    await expect(page).toHaveURL(/\/login\?return_to=%2Fproviders$/);
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();

    await page.getByLabel('Email').fill(owner.email);
    await page.getByLabel('Password').fill(owner.password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).toHaveURL(/\/providers$/);
    await expect(
      page.getByRole('heading', { name: 'Providers', exact: true })
    ).toBeVisible();

    await page.getByRole('button', { name: 'Open account menu' }).click();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await page.goto(
      `/invitations/accept#token=${encodeURIComponent(invitationToken!)}`
    );
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
    await expect(
      page.getByRole('heading', { name: 'Connect an upstream provider.' })
    ).toBeVisible();
    await page.getByRole('radio', { name: /Azure OpenAI/ }).check();
    await page.getByLabel('Provider name').fill(vertical.providerName);
    await page.getByLabel('Authentication').selectOption('api_key');
    await page.getByLabel('Seed model (optional)').fill(vertical.deployment);
    await page.getByLabel('Azure resource endpoint').fill(vertical.endpoint);
    await page.getByLabel('API version').fill(vertical.apiVersion);
    await page.getByLabel('Cloud deployment').fill(vertical.deployment);
    await page
      .getByLabel('Credential', { exact: true })
      .fill(vertical.credential);
    await page
      .getByRole('button', { name: /Save and test connection/ })
      .click();

    await expect(page.getByText(vertical.credential)).toHaveCount(0);
    await expect(
      page.getByRole('heading', { name: 'Discover upstream models' })
    ).toBeVisible();
    await page
      .getByRole('button', { name: 'Discover upstream models' })
      .click();
    await expect(
      page.getByRole('heading', { name: 'Review model capabilities' })
    ).toBeVisible();
    await expect(
      page.getByText(vertical.deployment, { exact: true }).first()
    ).toBeVisible();

    for (let index = 0; index < 4; index += 1) {
      await page.getByRole('button', { name: 'Add capability' }).click();
    }
    const surfaces: ClientSurface[] = ['openai', 'anthropic', 'gemini'];
    for (const [offset, surface] of surfaces.entries()) {
      const index = offset + 1;
      await page.getByLabel(`Operation ${index}`).selectOption('generation');
      await page.getByLabel(`Client surface ${index}`).selectOption(surface);
      await page.getByLabel(`Mode ${index}`).selectOption('unary');
    }
    await page.getByLabel('Operation 4').selectOption('generation');
    await page.getByLabel('Client surface 4').selectOption('openai');
    await page.getByLabel('Mode 4').selectOption('streaming');
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
    await expect(
      page.getByText('4/4 certified', { exact: true })
    ).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({
      path: test.info().outputPath('provider-onboarding.png'),
      fullPage: true
    });
    await page.getByRole('button', { name: 'Continue to activation' }).click();
    await page.getByRole('button', { name: 'Test completed draft' }).click();
    await expect(page.getByText(/Final draft test passed/)).toBeVisible();
    await expect(
      page.getByRole('button', { name: 'Activate provider' })
    ).toBeEnabled();
    await page.getByRole('button', { name: 'Activate provider' }).click();
    await expect(
      page.getByRole('heading', { name: 'Now build a stable route slug.' })
    ).toBeVisible();

    await page.getByRole('link', { name: 'Build default route' }).click();
    await expect(
      page.getByRole('heading', { name: 'Build a route draft.' })
    ).toBeVisible();
    await page.getByLabel('Public model slug').fill(vertical.route);
    await page.getByRole('button', { name: 'Add target' }).click();
    await expect(
      page.getByLabel('Provider model').first().locator('option:checked')
    ).toContainText(vertical.deployment);
    await page.getByLabel('Maximum attempts').fill('1');
    await page.getByRole('button', { name: 'Create draft' }).click();
    await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);
    await verifyDraftSave(page, 'route', vertical.route);

    await page.getByLabel('Dry-run operation').selectOption('generation');
    for (const surface of surfaces) {
      await page.getByLabel('Client surface').selectOption(surface);
      await page.getByLabel('Transport mode').selectOption('unary');
      await page.getByRole('button', { name: 'Simulate order' }).click();
      await expect(
        page.getByRole('heading', { name: 'Attempt explanation' })
      ).toBeVisible();
      await expect(
        page.getByText('Eligible in priority group 1')
      ).toBeVisible();
    }
    await page.getByRole('button', { name: 'Validate draft' }).click();
    await expect(page.getByText('Validation passed.')).toBeVisible();
    await page.getByRole('button', { name: 'Activate route' }).click();
    await expect(page.getByText('Revision 1 active')).toBeVisible();

    await page.goto('/api-keys/new');
    await expect(
      page.getByRole('heading', { name: 'Create a proxy key.' })
    ).toBeVisible();
    await page.getByLabel('Key name').fill(vertical.keyName);
    await expect(
      page.getByRole('checkbox', { name: 'Inference requests' })
    ).toBeChecked();
    await page.getByRole('checkbox', { name: 'Model listing' }).check();
    await page
      .getByRole('group', { name: 'Allowed route slugs' })
      .getByRole('checkbox', { name: vertical.route })
      .check();
    await page.getByRole('button', { name: /Create and show key/ }).click();

    const secretDialog = page.getByRole('dialog', {
      name: 'Copy this secret now.'
    });
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
        path: '/v1/responses'
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
      await secretDialog
        .getByRole('button', { name: 'Run connection test' })
        .click();
      expect((await gatewayResponse).status()).toBe(200);
      await expect(
        secretDialog.getByText(
          `${protocol.vendor} request succeeded through route ${vertical.route}.`
        )
      ).toBeVisible();
    }

    const observed = await observedGatewayResponses(page);
    expect(observed.map(({ path }) => path)).toEqual(
      protocolCases.map(({ path }) => path)
    );
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
      expect(new URLSearchParams(call.query).get('api-version')).toBe(
        vertical.apiVersion
      );
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

    await secretDialog
      .getByRole('button', { name: 'I have saved the key' })
      .click();
    await expect(secretDialog).toBeHidden();
    await page.goto('/requests');
    await expect(
      page.getByRole('heading', { name: 'Request Explorer' })
    ).toBeVisible();
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
      const href = await row
        .getByRole('link', { name: /^View request/ })
        .getAttribute('href');
      expect(href).toBeTruthy();
      requestLinks.set(surface, href!);
    }

    for (const surface of surfaces) {
      await page.goto(requestLinks.get(surface)!);
      await expect(
        page.getByRole('heading', { name: 'Request timeline' })
      ).toBeVisible();
      await expect(
        page.getByRole('heading', { name: vertical.route })
      ).toBeVisible();
      await expect(page.getByText('1 attempts', { exact: true })).toBeVisible();
      const facts = page.locator('section').filter({
        has: page.getByRole('heading', { name: vertical.route, exact: true })
      });
      const timeline = page.locator('section').filter({
        has: page.getByRole('heading', {
          name: 'Attempt timeline',
          exact: true
        })
      });
      await expectFact(facts, 'Operation', 'generation');
      await expectFact(facts, 'Client surface', surface);
      for (const scope of [facts, timeline]) {
        await expectFact(scope, 'Input tokens', '7');
        await expectFact(scope, 'Output tokens', '5');
        await expectFact(scope, 'Usage completeness', 'Complete');
      }
      await expect(
        page.getByText(vertical.providerName, { exact: true })
      ).toBeVisible();
      await expect(
        page.getByText(vertical.deployment, { exact: true })
      ).toBeVisible();
      await expectFact(
        timeline,
        'Response committed',
        'Yes — failover stopped'
      );
    }

    const streamed = await page.evaluate(
      async ({ apiKey, route }) => {
        const response = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: {
            Authorization: `Bearer ${apiKey}`,
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            model: route,
            messages: [{ role: 'user', content: 'Connection test' }],
            max_completion_tokens: 16,
            stream: true,
            stream_options: { include_usage: true }
          })
        });
        const reader = response.body!.getReader();
        const decoder = new TextDecoder();
        let body = '';
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          body += decoder.decode(value, { stream: true });
        }
        body += decoder.decode();
        return {
          status: response.status,
          contentType: response.headers.get('content-type'),
          body
        };
      },
      { apiKey: secret, route: vertical.route }
    );
    expect(streamed.status).toBe(200);
    expect(streamed.contentType).toContain('text/event-stream');
    expect(streamed.body).toContain(vertical.reply);
    expect(streamed.body.match(/data: \[DONE\]/g)).toHaveLength(1);
    await page.goto(`/requests?route=${vertical.route}&operation=generation`);
    await refreshUntilRequestCount(page, 4);
    await page.goto('/usage');
    await page.getByLabel('Route', { exact: true }).fill(vertical.route);
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    const requestMetric = page
      .locator('article')
      .filter({ has: page.getByText('Requests', { exact: true }) });
    await expect(requestMetric.locator('strong')).toHaveText('4');
  });

  test('API key secrets are shown once and the lifecycle converges against the real backend', async ({
    page,
    context
  }) => {
    page.on('dialog', (dialog) => dialog.accept());

    await signInAsOwner(page);

    const session = (await context.cookies(new URL(page.url()).origin)).filter(
      (cookie) => cookie.name.startsWith('__Host-')
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
    await page.screenshot({
      path: test.info().outputPath('budget-thresholds.png'),
      fullPage: true
    });
    await page.getByRole('button', { name: /Create and show key/ }).click();

    const created = page.getByRole('dialog', { name: 'Copy this secret now.' });
    await expect(created).toBeVisible();
    const secret = await takeSecret(created);
    // `CreateApiKeyResponse.secret` is documented "Returned only by this creation
    // response", so the value has to be real and then has to disappear.
    expect(secret).toMatch(/^olp_/);
    expect(
      (await new AxeBuilder({ page }).include('.secret-dialog').analyze())
        .violations
    ).toEqual([]);
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
    await page.keyboard.press('Escape');
    await expect(rotated).toBeHidden();
    await page.reload();
    await expectSecretGone(page, rotatedSecret, 'the rotated secret');

    await row.getByRole('button', { name: 'Revoke' }).click();
    await expect(row.getByText('revoked', { exact: true })).toBeVisible();

    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  });

  test('concurrent provider edits preserve local input until an explicit reload', async ({
    page,
    context
  }) => {
    await signInAsOwner(page);
    await page.goto('/providers');
    await page
      .getByRole('link', { name: vertical.providerName, exact: true })
      .click();
    await expect(page.getByLabel('Provider name')).toHaveValue(
      vertical.providerName
    );
    await verifyDraftSave(page, 'provider', vertical.providerName);
    const otherTab = await context.newPage();
    try {
      await otherTab.goto(page.url());
      await expect(otherTab.getByLabel('Provider name')).toHaveValue(
        vertical.providerName
      );
      await page
        .getByLabel('Provider name')
        .fill('Unsaved local provider name');
      await otherTab
        .getByLabel('Provider name')
        .fill('Concurrent provider name');
      await otherTab
        .getByRole('button', { name: 'Save draft', exact: true })
        .click();
      await expect(
        otherTab.getByText('Provider draft settings saved.', { exact: true })
      ).toBeVisible();
      await page
        .getByRole('button', { name: 'Save draft', exact: true })
        .click();
      await expect(
        page.getByText('This item changed elsewhere.', { exact: true })
      ).toBeVisible();
      await expect(page.getByLabel('Provider name')).toHaveValue(
        'Unsaved local provider name'
      );
      await page.getByRole('button', { name: 'Reload', exact: true }).click();
      await expect(page.getByLabel('Provider name')).toHaveValue(
        'Concurrent provider name'
      );
    } finally {
      await otherTab.close();
    }
  });
});

test('core console stays accessible and logout clears protected content in another tab', async ({
  page,
  context
}) => {
  test.skip(
    process.env.OLP_CONSOLE_E2E_RESTORED === 'true',
    'Covered on the fresh installation.'
  );
  test.setTimeout(180_000);
  await signInAsOwner(page);
  await verifyConsolePolish(page, test.info());
  const second = await context.newPage();
  await second.goto('/providers');
  await expect(
    second.getByRole('heading', { name: 'Providers', exact: true })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(second).toHaveURL(/\/login/);
  await expect(
    second.getByText(vertical.providerName, { exact: true })
  ).toHaveCount(0);
  await second.close();
});
