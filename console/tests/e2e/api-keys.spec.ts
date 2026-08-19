import AxeBuilder from '@axe-core/playwright';
import {
  denyClipboard,
  emulateTwoHundredPercentZoom,
  expect,
  mockSession,
  test
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import { ids, now, sessionOptions } from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('API key creation shows a secret once with SDK snippets on mobile', async ({
  page
}) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await denyClipboard(page);
  await mockSession(page, sessionOptions);
  let createBody: Record<string, unknown> | undefined;
  let createHeaders: Record<string, string> = {};
  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            id: ids.route,
            slug: 'default',
            created_at: now,
            revision_count: 1,
            latest_revision: {
              id: ids.revision,
              route_id: ids.route,
              revision: 1,
              slug: 'default',
              overall_timeout_ms: 120000,
              max_attempts: 1,
              source_draft_id: ids.draft,
              activated_by: ids.user,
              activated_at: now,
              operations: ['generation'],
              targets: []
            }
          }
        ],
        next_cursor: null
      }
    });
  });
  await page.route('**/api/v1/api-keys**', async (route) => {
    const request = route.request();
    if (request.method() === 'POST') {
      createBody = request.postDataJSON();
      createHeaders = await request.allHeaders();
      await route.fulfill({
        status: 201,
        json: {
          id: ids.key,
          lookup_id: 'olp_live_abcd',
          secret: 'olp_secret_shown_once',
          runtime_generation: { id: ids.generation, sequence: 4 }
        }
      });
      return;
    }
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/anthropic/v1/messages', async (route) => {
    expect(route.request().headers()['x-api-key']).toBe(
      'olp_secret_shown_once'
    );
    expect(route.request().postDataJSON()).toMatchObject({ model: 'default' });
    await route.fulfill({
      json: {
        id: 'msg_test',
        type: 'message',
        role: 'assistant',
        model: 'default',
        content: [{ type: 'text', text: 'ok' }],
        stop_reason: 'end_turn',
        usage: { input_tokens: 1, output_tokens: 1 }
      }
    });
  });
  await page.route(
    '**/gemini/v1beta/models/default:generateContent',
    async (route) => {
      expect(route.request().headers()['x-goog-api-key']).toBe(
        'olp_secret_shown_once'
      );
      expect(route.request().postDataJSON()).toEqual({
        contents: [{ role: 'user', parts: [{ text: 'Connection test' }] }],
        generationConfig: { maxOutputTokens: 16 }
      });
      await route.fulfill({
        json: {
          candidates: [
            {
              content: { role: 'model', parts: [{ text: 'ok' }] },
              finishReason: 'STOP'
            }
          ],
          usageMetadata: {
            promptTokenCount: 1,
            candidatesTokenCount: 1,
            totalTokenCount: 2
          }
        }
      });
    }
  );

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('mobile-app');
  await page.getByLabel('Requests per minute').fill('120');
  await page.getByLabel('Concurrent requests').fill('8');
  await page
    .getByRole('group', { name: 'Allowed route slugs' })
    .getByRole('checkbox', { name: 'default' })
    .check();
  await page.getByRole('button', { name: /Create and show key/ }).click();

  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(dialog).toBeVisible();
  await expect(
    dialog.getByText('olp_secret_shown_once', { exact: true })
  ).toBeVisible();
  await expect(dialog.getByText('base_url=')).toBeVisible();
  await dialog.getByRole('button', { name: 'Copy key' }).click();
  await expect(dialog.getByRole('alert')).toContainText(
    'Clipboard access is unavailable. Copy the value manually.'
  );
  expect(
    (await new AxeBuilder({ page }).include('.secret-dialog').analyze())
      .violations
  ).toEqual([]);
  expect(createBody).toMatchObject({
    name: 'mobile-app',
    allowed_routes: ['default'],
    requests_per_minute: 120,
    max_concurrency: 8
  });
  expect(createHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(createHeaders['x-csrf-token']).toBe('csrf-e2e');

  await dialog.getByRole('tab', { name: 'Anthropic TS' }).click();
  await expect(dialog.getByText('client.messages.create')).toBeVisible();
  await dialog.getByRole('button', { name: 'Run connection test' }).click();
  await expect(
    dialog.getByText('Anthropic request succeeded through route default.')
  ).toBeVisible();
  await dialog.getByRole('tab', { name: 'Gemini TS' }).click();
  await expect(dialog.getByRole('tabpanel')).toContainText(
    'baseUrl: "http://127.0.0.1:4174/gemini"'
  );
  await expect(dialog.getByRole('tabpanel')).toContainText(
    'apiVersion: "v1beta"'
  );
  await dialog.getByRole('button', { name: 'Run connection test' }).click();
  await expect(
    dialog.getByText('Gemini request succeeded through route default.')
  ).toBeVisible();
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(page).toHaveURL(/\/api-keys$/);
  await expect(page.getByText('olp_secret_shown_once')).toHaveCount(0);
});

test('API key policy updates, rotation, and revocation converge in the list', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  page.on('dialog', (dialog) => dialog.accept());
  let revokedAt: string | null = null;
  let keyName = 'production SDK';
  let requestsPerMinute = 120;
  let keyEtag = '01980000-0000-7000-8000-000000000309';
  const keyRecord = () => ({
    id: ids.key,
    lookup_id: 'olp_live_abcd',
    name: keyName,
    scopes: ['inference'],
    allowed_routes: ['default'],
    requests_per_minute: requestsPerMinute,
    tokens_per_minute: null,
    max_concurrency: 8,
    expires_at: null,
    revoked_at: revokedAt,
    rotated_at: null,
    etag: keyEtag,
    created_at: now
  });

  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            id: ids.route,
            slug: 'default',
            created_at: now,
            revision_count: 1,
            latest_revision: {
              id: ids.revision,
              route_id: ids.route,
              revision: 1,
              slug: 'default',
              overall_timeout_ms: 120000,
              max_attempts: 1,
              source_draft_id: ids.draft,
              activated_by: ids.user,
              activated_at: now,
              operations: ['generation'],
              targets: []
            }
          }
        ],
        next_cursor: null
      }
    });
  });

  await page.route('**/api/v1/api-keys**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/rotate')) {
      await route.fulfill({
        json: {
          id: ids.key,
          lookup_id: 'olp_live_efgh',
          secret: 'rotated-key-shown-once',
          etag: keyRecord().etag,
          runtime_generation: { id: ids.generation, sequence: 5 }
        }
      });
      return;
    }
    if (pathname.endsWith('/revoke')) {
      revokedAt = now;
      await route.fulfill({ json: { id: ids.generation, sequence: 6 } });
      return;
    }
    if (request.method() === 'PATCH') {
      const body = request.postDataJSON() as {
        name: string;
        requests_per_minute: number;
      };
      keyName = body.name;
      requestsPerMinute = body.requests_per_minute;
      keyEtag = '01980000-0000-7000-8000-000000000310';
      await route.fulfill({
        json: {
          etag: keyEtag,
          runtime_generation: { id: ids.generation, sequence: 5 }
        }
      });
      return;
    }
    await route.fulfill({ json: { items: [keyRecord()], next_cursor: null } });
  });

  await page.goto('/api-keys');
  await page.getByRole('button', { name: 'Edit' }).click();
  await page.getByLabel('Key name').fill('renamed SDK');
  await page.getByLabel('Requests per minute').fill('240');
  await page.getByRole('button', { name: 'Save and publish' }).click();
  await expect(page.getByText('renamed SDK', { exact: true })).toBeVisible();
  await expect(page.getByText(/240 RPM/)).toBeVisible();
  await page.getByRole('button', { name: 'Rotate' }).click();
  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(
    dialog.getByText('rotated-key-shown-once', { exact: true })
  ).toBeVisible();
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(page.getByText('rotated-key-shown-once')).toHaveCount(0);
  await page.getByRole('button', { name: 'Revoke' }).click();
  await expect(page.getByText('revoked', { exact: true })).toBeVisible();
});

test('API key creation with transport failure retains key and payload, recovering secret on retry without storage leaks', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let postCount = 0;
  let firstKey: string | null = null;
  let secondKey: string | null = null;

  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({
      json: { items: [], next_cursor: null }
    });
  });

  await page.route('**/api/v1/api-keys**', async (route) => {
    const request = route.request();
    if (request.method() === 'POST') {
      postCount += 1;
      const key = request.headers()['idempotency-key'];
      if (postCount === 1) {
        firstKey = key;
        await route.abort('failed');
        return;
      }
      secondKey = key;
      await route.fulfill({
        status: 201,
        json: {
          id: ids.key,
          lookup_id: 'olp_live_retry',
          secret: 'olp_secret_recovered_on_retry',
          runtime_generation: { id: ids.generation, sequence: 7 }
        }
      });
      return;
    }
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });

  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('retryable-app');
  await page.getByLabel('Requests per minute').fill('100');
  await page.getByLabel('Concurrent requests').fill('5');
  await page.getByRole('button', { name: /Create and show key/ }).click();

  // Indeterminate outcome shows error guidance
  await expect(page.getByRole('alert')).toContainText('Outcome unknown');
  await expect(page.getByRole('alert')).toContainText('Retry safely');

  // Retrying dispatches the mutation again
  await page.getByRole('button', { name: /Create and show key/ }).click();

  // Secret is displayed seamlessly in the dialog
  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(dialog).toBeVisible();
  await expect(
    dialog.getByText('olp_secret_recovered_on_retry', { exact: true })
  ).toBeVisible();

  // Verify same idempotency key was sent across attempts
  expect(postCount).toBe(2);
  expect(firstKey).toBeTruthy();
  expect(firstKey).toBe(secondKey);

  // Verify storage is clean
  const storage = await page.evaluate(() => {
    const filterFramework = (store: Storage) =>
      Object.fromEntries(
        Object.entries(store).filter(([k]) => !k.startsWith('sveltekit:'))
      );
    return {
      local: filterFramework(localStorage),
      session: filterFramework(sessionStorage)
    };
  });
  expect(storage.local).toEqual({});
  expect(storage.session).toEqual({});
});
