import AxeBuilder from '@axe-core/playwright';
import { expect, mockSession, test } from '../playwright';

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/routes?*', async (route) => {
    await route.fulfill({
      json: {
        items: [
          { id: '01980000-0000-7000-8000-000000000201', slug: 'support-chat' }
        ],
        next_cursor: null
      }
    });
  });
});

test('playground loads active route pages and submits a listed slug', async ({
  page
}) => {
  await mockSession(page);
  const response = Promise.withResolvers<void>();
  const cursors: Array<string | null> = [];
  await page.route('**/api/v1/routes?*', async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    cursors.push(cursor);
    await response.promise;
    await route.fulfill({
      json: {
        items: [
          {
            id: cursor ? 'second-route' : 'first-route',
            slug: cursor ? 'research-chat' : 'support-chat'
          }
        ],
        next_cursor: cursor ? null : 'next-routes'
      }
    });
  });
  let submitted: Record<string, unknown> = {};
  await page.route('**/api/v1/playground', async (route) => {
    submitted = route.request().postDataJSON();
    await route.fulfill({
      json: {
        id: 'resp_route_choice',
        model: submitted.model,
        output_text: 'Selected route answered.',
        latency_ms: 12
      }
    });
  });
  try {
    await page.goto('/playground');
    await expect(page.getByRole('status')).toHaveText('Loading active routes…');
    const slug = page.getByLabel('Route slug');
    await expect(slug).toBeEditable();
    await expect(slug).toHaveAttribute('list', 'playground-routes');
    response.resolve();
    const options = page.locator('#playground-routes option');
    await expect(options).toHaveCount(2);
    await expect(options.nth(0)).toHaveAttribute('value', 'support-chat');
    await expect(options.nth(1)).toHaveAttribute('value', 'research-chat');
    expect(cursors).toEqual([null, 'next-routes']);
    await slug.fill((await options.nth(1).getAttribute('value'))!);
    await page.getByLabel('Prompt').fill('Hello.');
    await page.getByRole('button', { name: 'Run test' }).click();
    await expect(page.getByText('Selected route answered.')).toBeVisible();
    expect(submitted.model).toBe('research-chat');
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  } finally {
    response.resolve();
  }
});

test('playground keeps manual slug entry when no active routes are listed', async ({
  page
}) => {
  await mockSession(page);
  await page.route('**/api/v1/routes?*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  let submitted: Record<string, unknown> = {};
  await page.route('**/api/v1/playground', async (route) => {
    submitted = route.request().postDataJSON();
    await route.fulfill({
      status: 400,
      json: {
        status: 400,
        title: 'This route does not support the requested operation.'
      }
    });
  });
  await page.goto('/playground');
  await expect(
    page.getByText(
      'No active routes are available. Activate a route to get started.'
    )
  ).toBeVisible();
  await expect(page.locator('#playground-routes option')).toHaveCount(0);
  await page.getByLabel('Route slug').fill(' newly-activated-route ');
  await page.getByLabel('Prompt').fill('Hello.');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.getByRole('alert')).toHaveText(
    'This route does not support the requested operation.'
  );
  expect(submitted.model).toBe('newly-activated-route');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('playground can retry failed route choices without losing a manual slug', async ({
  page
}) => {
  await mockSession(page);
  const retryResponse = Promise.withResolvers<void>();
  let retry = false;
  await page.route('**/api/v1/routes?*', async (route) => {
    if (!retry) {
      await route.fulfill({
        status: 503,
        json: { status: 503, title: 'Route inventory is unavailable.' }
      });
      return;
    }
    await retryResponse.promise;
    await route.fulfill({
      json: {
        items: [{ id: 'recovered-route', slug: 'support-chat' }],
        next_cursor: null
      }
    });
  });
  try {
    await page.goto('/playground');
    await expect(page.getByRole('alert')).toHaveText(
      'Route inventory is unavailable.'
    );
    await page.getByLabel('Route slug').fill('manual-chat');
    await expect(
      page.getByText('You can still enter a route slug.')
    ).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
    retry = true;
    await page.getByRole('button', { name: 'Retry routes' }).click();
    await expect(page.getByRole('status')).toHaveText('Loading active routes…');
    retryResponse.resolve();
    await expect(page.locator('#playground-routes option')).toHaveAttribute(
      'value',
      'support-chat'
    );
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByLabel('Route slug')).toHaveValue('manual-chat');
  } finally {
    retryResponse.resolve();
  }
});

test('playground sends an ephemeral session-authorized structured-output request', async ({
  page
}) => {
  await mockSession(page);
  let headers: Record<string, string> = {};
  let payload: Record<string, unknown> = {};
  await page.route('**/api/v1/playground', async (route) => {
    headers = route.request().headers();
    payload = route.request().postDataJSON() as Record<string, unknown>;
    await route.fulfill({
      json: {
        id: 'resp_test',
        model: 'support-chat',
        provider_model: 'gpt-test',
        finish_reason: 'stop',
        output_text: null,
        tool_calls: null,
        structured_output: { answer: 'Safe and ephemeral' },
        usage: {
          input_tokens: 12,
          cached_input_tokens: 8,
          output_tokens: 4,
          reasoning_tokens: 2,
          total_tokens: 16
        },
        latency_ms: 142
      }
    });
  });
  await page.goto('/playground');
  await page.getByRole('radio', { name: 'Text' }).focus();
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('ArrowRight');
  await expect(
    page.getByRole('radio', { name: 'Structured output' })
  ).toBeChecked();
  await page.getByLabel('Route slug').fill('support-chat');
  await page.getByLabel('Client surface').selectOption('anthropic');
  await page.getByLabel('Prompt').fill('Return a structured answer.');
  await page.getByLabel('Temperature').fill('0.2');
  await page.getByLabel('Max output tokens').fill('256');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.getByText('Safe and ephemeral')).toBeVisible();
  await expect(page.getByText('Provider model').locator('..')).toContainText(
    'gpt-test'
  );
  await expect(page.getByText('Finish reason').locator('..')).toContainText(
    'stop'
  );
  await expect(
    page.getByText('Cached input tokens').locator('..')
  ).toContainText('8');
  await expect(page.getByText('Reasoning tokens').locator('..')).toContainText(
    '2'
  );
  await expect(page.getByText('Total tokens').locator('..')).toContainText(
    '16'
  );
  expect(payload).toMatchObject({
    model: 'support-chat',
    surface: 'anthropic',
    input: 'Return a structured answer.',
    temperature: 0.2,
    max_output_tokens: 256
  });
  expect(payload).not.toHaveProperty('stream');
  expect(headers.authorization).toBeUndefined();
  // WebKit serializes Fetch's `cache: 'no-store'` mode as `no-cache` on the
  // wire; both values forbid reuse by the browser cache.
  expect(['no-store', 'no-cache']).toContain(headers['cache-control']);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('playground shows a refusal instead of an empty result', async ({
  page
}) => {
  await mockSession(page);
  await page.route('**/api/v1/playground', async (route) => {
    await route.fulfill({
      json: {
        id: 'resp_refusal',
        model: 'support-chat',
        provider_model: 'gpt-test',
        finish_reason: 'refusal',
        output_text: '',
        tool_calls: [],
        structured_output: null,
        refusal: 'I cannot help with that request.',
        usage: { input_tokens: 9, output_tokens: 0, total_tokens: 9 },
        latency_ms: 88
      }
    });
  });

  await page.goto('/playground');
  await page.getByLabel('Route slug').fill('support-chat');
  await page.getByLabel('Prompt').fill('Do something disallowed.');
  await page.getByRole('button', { name: 'Run test' }).click();
  const refusal = page.getByRole('alert');
  await expect(refusal).toContainText('The model refused this request');
  await expect(refusal).toContainText('I cannot help with that request.');
  await expect(page.getByText('No content returned')).toHaveCount(0);
  await expect(page.getByText('Finish reason').locator('..')).toContainText(
    'refusal'
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('playground refuses an out-of-range temperature before sending it', async ({
  page
}) => {
  await mockSession(page);
  let sent = 0;
  await page.route('**/api/v1/playground', async (route) => {
    sent += 1;
    await route.fulfill({
      json: {
        id: 'resp_test',
        model: 'support-chat',
        output_text: 'ok',
        tool_calls: [],
        latency_ms: 10
      }
    });
  });

  await page.goto('/playground');
  await page.getByLabel('Route slug').fill('support-chat');
  await page.getByLabel('Prompt').fill('Hello.');
  await page.getByLabel('Temperature').fill('3');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(
    page.getByText('Temperature must be from 0 through 2.')
  ).toBeVisible();
  expect(sent).toBe(0);
});
