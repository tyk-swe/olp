import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test, type Page } from '../playwright';

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
  const provider = await manage(page, 'POST', '/api/v1/providers', {
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
    `/api/v1/providers/${providerId}/discovery`,
    {
      match: `/api/v1/providers/${providerId}`,
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
  const draftA = await manage(page, 'POST', '/api/v1/route-drafts', {
    idempotency: crypto.randomUUID(),
    body: draftBody('lifetime-alpha')
  });
  expect(draftA.status).toBe(201);
  const draftB = await manage(page, 'POST', '/api/v1/route-drafts', {
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
  await page.route(`**/api/v1/route-drafts/${idA}`, async (route) => {
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
          const response = await fetch(`/api/v1/route-drafts/${id}`);
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
    await page.unroute(`**/api/v1/route-drafts/${idA}`);
  }
  await page.screenshot({
    path: info.outputPath('route-resource-lifetime.png'),
    fullPage: true
  });
});
