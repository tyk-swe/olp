import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '../playwright';

// The owner created by tests/access/control.spec.ts; a run that starts on an
// empty installation performs the setup itself.
const owner = {
  email: 'owner@example.com',
  password: 'a long browser test password'
};

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
          new URL(response.url()).pathname === '/api/v3/sessions'
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
): Promise<{ status: number; body: Record<string, unknown> }> {
  return page.evaluate(
    async ({ method, path, options }) => {
      const session = await fetch('/api/v3/sessions/current').then((r) =>
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

/// Client-side navigation through the router: an in-document anchor click is
/// intercepted by SvelteKit, so the [routeId] page stays mounted and only its
/// parameter changes — a full load would hide component reuse.
async function clientNavigate(page: Page, href: string) {
  await page.evaluate((href) => {
    const link = document.createElement('a');
    link.href = href;
    document.body.append(link);
    link.click();
    link.remove();
  }, href);
}

test('route editor state stays scoped to the draft being viewed across parameter navigation', async ({
  page
}, info) => {
  test.setTimeout(150_000);
  await signIn(page);

  // Seed one provider model and two drafts through the management API.
  const provider = await manage(page, 'POST', '/api/v3/providers', {
    idempotency: crypto.randomUUID(),
    body: {
      name: 'Lifetime provider',
      configuration: { kind: 'openai', auth_mode: 'none' }
    }
  });
  expect(provider.status).toBe(201);
  const providerId = provider.body.id as string;
  const discovered = await manage(
    page,
    'POST',
    `/api/v3/providers/${providerId}/discovery`,
    {
      match: `/api/v3/providers/${providerId}`,
      body: {
        models: [
          { upstream_model: 'lifetime-model', display_name: 'Lifetime model' }
        ]
      }
    }
  );
  expect(discovered.status).toBe(200);
  const draftBody = (slug: string) => ({
    slug,
    operations: ['generation'],
    overall_timeout_ms: 120000,
    max_attempts: 1,
    targets: [
      {
        provider_id: providerId,
        provider_model: 'lifetime-model',
        priority: 1,
        weight: 1,
        timeout_ms: 60000
      }
    ]
  });
  const draftA = await manage(page, 'POST', '/api/v3/route-drafts', {
    idempotency: crypto.randomUUID(),
    body: draftBody('lifetime-alpha')
  });
  expect(draftA.status).toBe(201);
  const draftB = await manage(page, 'POST', '/api/v3/route-drafts', {
    idempotency: crypto.randomUUID(),
    body: draftBody('lifetime-beta')
  });
  expect(draftB.status).toBe(201);
  const idA = draftA.body.id as string;
  const idB = draftB.body.id as string;

  const slug = page.getByLabel('Public model slug');
  const savedNotice = page.getByText('Draft saved.', { exact: false });
  const conflictNotice = page.getByText('This item changed elsewhere.', {
    exact: true
  });
  const saveButton = page.getByRole('button', {
    name: 'Save draft',
    exact: true
  });

  await page.goto(`/routes/${idA}`);
  await expect(slug).toHaveValue('lifetime-alpha');

  // Cancelled navigation keeps A's unsaved input.
  await slug.fill('lifetime-alpha-local');
  const dialogSeen = Promise.withResolvers<string>();
  page.once('dialog', async (dialog) => {
    dialogSeen.resolve(dialog.message());
    await dialog.dismiss();
  });
  await clientNavigate(page, `/routes/${idB}`);
  expect(await dialogSeen.promise).toBe('Discard unsaved changes?');
  await expect(page).toHaveURL(new RegExp(`/routes/${idA}$`));
  await expect(slug).toHaveValue('lifetime-alpha-local');

  // Accepted navigation hydrates B from B's data only.
  page.once('dialog', (dialog) => dialog.accept());
  await clientNavigate(page, `/routes/${idB}`);
  await expect(page).toHaveURL(new RegExp(`/routes/${idB}$`));
  await expect(slug).toHaveValue('lifetime-beta');
  await expect(savedNotice).toHaveCount(0);
  await expect(conflictNotice).toHaveCount(0);
  await expect(page.locator('[role="alert"]')).toHaveCount(0);
  await expect(saveButton).toBeEnabled();

  // A save in flight when the user leaves A must not touch B's editor.
  await clientNavigate(page, `/routes/${idA}`);
  await expect(slug).toHaveValue('lifetime-alpha');
  await slug.fill('lifetime-alpha-saved');
  const release = Promise.withResolvers<void>();
  const sawSave = Promise.withResolvers<void>();
  await page.route(`**/api/v3/route-drafts/${idA}`, async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    sawSave.resolve();
    await release.promise;
    await route.continue();
  });
  try {
    await saveButton.click();
    await sawSave.promise;
    const saveDialog = Promise.withResolvers<string>();
    page.once('dialog', async (dialog) => {
      saveDialog.resolve(dialog.message());
      await dialog.accept();
    });
    await clientNavigate(page, `/routes/${idB}`);
    expect(await saveDialog.promise).toBe('Discard unsaved changes?');
    await expect(slug).toHaveValue('lifetime-beta');
    release.resolve();
    await expect
      .poll(async () => {
        const refreshed = await page.evaluate(async (id) => {
          const response = await fetch(`/api/v3/route-drafts/${id}`);
          return response.ok
            ? ((await response.json()) as { slug: string }).slug
            : '';
        }, idA);
        return refreshed;
      })
      .toBe('lifetime-alpha-saved');
    // The committed write landed on A; B's editor shows none of it.
    await expect(slug).toHaveValue('lifetime-beta');
    await expect(savedNotice).toHaveCount(0);
    await expect(conflictNotice).toHaveCount(0);
    await expect(page.locator('[role="alert"]')).toHaveCount(0);
    await expect(saveButton).toBeEnabled();
  } finally {
    release.resolve();
    await page.unroute(`**/api/v3/route-drafts/${idA}`);
  }
  await page.screenshot({
    path: info.outputPath('route-resource-lifetime.png'),
    fullPage: true
  });
});
