import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '../playwright';
import { waitForRoutePublication } from '../journeys/fixtures';

const fixture = 'http://127.0.0.1:4188';
const providerSecret = 'anthropic-browser-fixture-secret';
const expectedNext = JSON.parse(
  readFileSync(
    new URL(
      '../../../tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
      import.meta.url
    ),
    'utf8'
  )
);
type ManagementBody = Record<string, unknown> & {
  id: string;
  etag: string;
  secret: string;
  items: { id: string }[];
};

async function signIn(page: Page) {
  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (
    await page.getByRole('button', { name: 'Create owner account' }).isVisible()
  ) {
    await page.getByLabel('Display name').fill('Owner');
    await page.getByLabel('Work email').fill('owner@example.com');
    await page
      .getByLabel('Password', { exact: true })
      .fill('a long browser test password');
    await page
      .getByLabel('Confirm password')
      .fill('a long browser test password');
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
    await page.getByRole('button', { name: 'Create owner account' }).click();
  } else {
    await page.getByLabel('Email').fill('owner@example.com');
    await page
      .getByLabel('Password', { exact: true })
      .fill('a long browser test password');
    await page.getByRole('button', { name: 'Sign in' }).click();
  }
  await expect(page).toHaveURL(/\/$/);
}

async function management(
  page: Page,
  method: 'GET' | 'POST' | 'PATCH',
  path: string,
  body?: unknown,
  match?: string
): Promise<{ status: number; body: ManagementBody }> {
  return page.evaluate(
    async ({ method, path, body, match }) => {
      const session = await fetch('/api/v3/sessions/current').then((response) =>
        response.json()
      );
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token
      };
      if (method === 'POST') headers['Idempotency-Key'] = crypto.randomUUID();
      if (match) headers['If-Match'] = `"${match}"`;
      const response = await fetch(path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body)
      });
      return { status: response.status, body: await response.json() };
    },
    { method, path, body, match }
  ) as Promise<{ status: number; body: ManagementBody }>;
}

function success(
  result: { status: number; body: ManagementBody },
  status: number
) {
  expect(result.status, JSON.stringify(result.body)).toBe(status);
  return result.body;
}

test('strict inspector has zero inference effects and browser tool continuation preserves the native next request', async ({
  page,
  request
}, info) => {
  test.setTimeout(240_000);
  await signIn(page);
  const slug = `strict-browser-${info.project.name}`;
  const provider = success(
    await management(page, 'POST', '/api/v3/providers', {
      name: `Browser Anthropic ${info.project.name}`,
      configuration: {
        kind: 'anthropic',
        profile_id: 'anthropic-messages',
        profile_revision: '1',
        endpoint: `${fixture}/v1`,
        auth_mode: 'api_key',
        options: {
          operation_defaults: {
            generation: {
              dialect: 'anthropic-messages',
              values: {
                max_tokens: 2048,
                thinking: { type: 'enabled', budget_tokens: 1024 }
              }
            }
          }
        }
      },
      model: 'fixture-model',
      credential: providerSecret
    }),
    201
  );
  const providerPath = `/api/v3/providers/${provider.id}`;
  const models = success(
    await management(page, 'GET', `${providerPath}/models`),
    200
  );
  const reviewed = success(
    await management(
      page,
      'PATCH',
      `${providerPath}/models/${models.items[0].id}`,
      {
        enabled: true,
        capabilities: ['openai', 'anthropic'].flatMap((surface) =>
          ['unary', 'streaming'].map((mode) => ({
            operation: 'generation',
            surface,
            mode
          }))
        )
      },
      provider.etag
    ),
    200
  );
  success(
    await management(
      page,
      'POST',
      `${providerPath}/models/${models.items[0].id}/certify`,
      undefined,
      reviewed.etag
    ),
    200
  );
  const current = success(await management(page, 'GET', providerPath), 200);
  success(
    await management(
      page,
      'POST',
      `${providerPath}/activate`,
      undefined,
      current.etag
    ),
    200
  );
  const draft = success(
    await management(page, 'POST', '/api/v3/route-drafts', {
      slug,
      operations: ['generation'],
      overall_timeout_ms: 20_000,
      max_attempts: 1,
      fidelity: { mode: 'strict' },
      targets: [
        {
          provider_id: provider.id,
          provider_model: 'fixture-model',
          priority: 0,
          weight: 1,
          timeout_ms: 15_000
        }
      ]
    }),
    201
  );
  success(
    await management(
      page,
      'POST',
      `/api/v3/route-drafts/${draft.id}/activate`,
      undefined,
      draft.etag
    ),
    200
  );
  const key = success(
    await management(page, 'POST', '/api/v3/api-keys', {
      name: `Browser continuation ${info.project.name}`,
      scopes: ['inference', 'models_read'],
      allowed_routes: [slug],
      allow_provider_state: true
    }),
    201
  );
  const secret = key.secret as string;
  expect(secret).toMatch(/^olp_/);
  await waitForRoutePublication(page, secret, slug);

  expect((await request.post(`${fixture}/__test__/reset`)).status()).toBe(204);
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(slug);
  await expect(
    page.getByText(/requires a client that retains its native observation/)
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Run test' })).toBeDisabled();
  await page.getByRole('radio', { name: 'Advanced' }).check();
  await page.getByLabel('Client surface').selectOption('anthropic');
  await page.getByLabel('Request JSON').fill(
    JSON.stringify({
      model: slug,
      max_tokens: 2048,
      messages: [{ role: 'user', content: 'private-prompt-marker' }]
    })
  );
  await page.getByText('Inspect effective plan without running').click();
  await page
    .getByLabel('Native request dialect')
    .selectOption('anthropic-messages');
  await page.getByRole('button', { name: 'Inspect plan' }).click();
  const explanation = page.getByLabel('Effective interaction plan');
  await expect(explanation).toBeVisible();
  await explanation
    .getByText('Inspect request, return path, and obligations')
    .click();
  await expect(
    explanation.getByText('anthropic-messages').first()
  ).toBeVisible();
  await expect(explanation.getByText(/messages\s+1 · user/)).toBeVisible();
  await expect(explanation).not.toContainText('private-prompt-marker');
  const afterInspection = await request
    .get(`${fixture}/__test__/requests`)
    .then((response) => response.json());
  expect(afterInspection.calls).toHaveLength(0);
  await explanation.screenshot({
    path: info.outputPath('strict-effective-plan.png')
  });

  await page.getByLabel('Client surface').selectOption('openai');
  await page.getByLabel('Template').selectOption('generation-negotiated-tools');
  await page
    .getByLabel('Request JSON')
    .fill(
      (await page.getByLabel('Request JSON').inputValue()).replace(
        '"model": ""',
        `"model": "${slug}"`
      )
    );
  await expect(
    page.getByRole('heading', { name: 'Complete a strict tool interaction' })
  ).toBeVisible();
  await page
    .getByLabel('Inference API key with provider-state permission')
    .fill(secret);
  const unnegotiated = await page.evaluate(
    async ({ secret, slug }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${secret}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          messages: [{ role: 'user', content: 'Weather and time in Paris?' }],
          tools: [
            {
              type: 'function',
              function: { name: 'weather', parameters: { type: 'object' } }
            }
          ]
        })
      });
      return { status: response.status, body: await response.json() };
    },
    { secret, slug }
  );
  expect(unnegotiated.status).toBe(400);
  expect(unnegotiated.body.error.code).toBe('state_carrier');
  const afterRefusal = await request
    .get(`${fixture}/__test__/requests`)
    .then((response) => response.json());
  expect(afterRefusal.calls).toHaveLength(0);

  await page.getByRole('button', { name: 'Run strict first turn' }).click();
  await expect(page.getByText('Recoverable continuation ready')).toBeVisible();
  await expect(page.getByText('Call 1 · weather')).toBeVisible();
  await expect(page.getByText('Call 2 · clock')).toBeVisible();
  await expect(page.getByText('beforeafter')).toBeVisible();
  await expect(
    page.getByText('opaque-fixture-signature-do-not-log')
  ).toHaveCount(0);
  await page.getByLabel('Result for call 1').fill('sunny');
  await page.getByLabel('Result for call 2').fill('14:00');
  await page
    .getByRole('button', { name: 'Submit ordered tool results' })
    .click();
  await expect(page.locator('.assistant-result pre')).toHaveText(
    'Both tools completed.'
  );
  await expect(page.getByText(secret, { exact: true })).toHaveCount(0);
  const captured = await request
    .get(`${fixture}/__test__/requests`)
    .then((response) => response.json());
  expect(captured.unexpected).toEqual([]);
  expect(captured.calls).toHaveLength(2);
  assert.deepEqual(captured.calls[1].body, expectedNext);
  expect(captured.calls[0].headers['x-api-key']).toBe(providerSecret);
  expect(captured.calls[1].headers['x-api-key']).toBe(providerSecret);
  expect(JSON.stringify(captured)).not.toContain(secret);
  await page
    .getByRole('heading', { name: 'Complete a strict tool interaction' })
    .locator('..')
    .screenshot({
      path: info.outputPath('strict-tool-continuation.png'),
      animations: 'disabled'
    });
});

test('native vector and rerank presentation keeps storage and score representation', async ({
  page
}, info) => {
  await signIn(page);
  const vector =
    '{"data":[{"index":0,"embedding":"AP8="}],"provider_metadata":{"count":9007199254740993}}';
  const ranked =
    '{"results":[{"index":1,"relevance_score":0.1000000000000000000001},{"index":0,"relevance_score":0.1000000000000000000001}]}';
  await page.route('**/api/v3/playground', async (route) => {
    const request = JSON.parse(route.request().postData() ?? '{}') as {
      operation?: string;
    };
    const result = request.operation === 'rerank' ? ranked : vector;
    await route.fulfill({
      status: 200,
      headers: {
        'content-type': 'application/json',
        'cache-control': 'no-store'
      },
      body: `{"id":"00000000-0000-4000-8000-000000000001","model":"display-route","output_text":"","tool_calls":[],"latency_ms":1,"routing":[],"response":${result},"response_raw":${JSON.stringify(result)}}`
    });
  });
  await page.goto('/playground');
  await page.getByRole('radio', { name: 'Advanced' }).check();
  await page.getByLabel('Route slug').fill('display-route');
  await page.getByLabel('Operation').selectOption('embeddings');
  await page
    .getByLabel('Request JSON')
    .fill(
      '{"model":"display-route","input":"one","output_dtype":"ubinary","output_dimension":16,"encoding_format":"base64"}'
    );
  await page.getByRole('button', { name: 'Run test' }).click();
  const result = page.locator('.operation-result');
  await expect(result.getByText('Base64 storage · ubinary')).toBeVisible();
  await expect(result.getByText('2 stored bytes')).toBeVisible();
  await result.getByText('Native result JSON').click();
  await expect(
    result.locator('[data-testid="native-operation-result"]')
  ).toContainText('9007199254740993');
  await result.screenshot({ path: info.outputPath('native-vector-shape.png') });

  await page.getByLabel('Operation').selectOption('rerank');
  await page
    .getByLabel('Request JSON')
    .fill(
      '{"model":"display-route","query":"rank","documents":[{"id":"alpha","text":"a"},{"id":"beta","text":"b"}]}'
    );
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(
    result.locator('td code').filter({ hasText: '0.1000000000000000000001' })
  ).toHaveCount(2);
  await expect(result.getByText('"beta"')).toBeVisible();
  await expect(result.getByText('"alpha"')).toBeVisible();
  await result.screenshot({
    path: info.outputPath('native-rerank-scores.png')
  });
});
