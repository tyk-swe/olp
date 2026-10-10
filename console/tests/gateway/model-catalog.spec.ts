import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from './signIn';
import { manage } from '../helpers/management';

test('catalog discovery, owner publishing and explicit upstream disclosure', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    {
      body: { name: 'Catalog browser project' },
      idempotency: 'catalog-browser-project'
    }
  );
  expect(project.status).toBe(201);
  const provider = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      body: {
        name: 'Private catalog browser provider',
        project_id: project.body.id,
        configuration: {
          kind: 'openai_compatible',
          auth_mode: 'api_key',
          endpoint: 'http://127.0.0.1:4187/v1'
        },
        credential: 'compatible-provider-secret',
        model: 'compatible-e2e-model'
      },
      idempotency: 'catalog-browser-provider'
    }
  );
  expect(provider.status).toBe(201);
  const providerPath = `/api/v1/providers/${provider.body.id}`;
  expect(
    (
      await manage(page, 'POST', `${providerPath}/probe`, {
        match: providerPath
      })
    ).status
  ).toBe(200);
  const models = await manage<{ items: Array<{ id: string }> }>(
    page,
    'GET',
    `${providerPath}/models`
  );
  expect(
    (
      await manage(
        page,
        'PATCH',
        `${providerPath}/models/${models.body.items[0].id}`,
        {
          body: {
            enabled: true,
            capabilities: [
              { operation: 'generation', surface: 'openai', mode: 'unary' },
              { operation: 'generation', surface: 'openai', mode: 'streaming' }
            ]
          },
          match: providerPath
        }
      )
    ).status
  ).toBe(200);
  expect(
    (
      await manage(
        page,
        'POST',
        `${providerPath}/models/${models.body.items[0].id}/certify`,
        {
          match: providerPath
        }
      )
    ).status
  ).toBe(200);
  expect(
    (
      await manage(page, 'POST', `${providerPath}/activate`, {
        match: providerPath,
        idempotency: 'catalog-browser-activate-provider'
      })
    ).status
  ).toBe(200);
  const draft = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/route-drafts',
    {
      body: {
        slug: 'catalog-browser',
        project_id: project.body.id,
        operations: ['generation'],
        overall_timeout_ms: 30000,
        max_attempts: 1,
        fidelity: { mode: 'transformed' },
        targets: [
          {
            provider_id: provider.body.id,
            provider_model: 'compatible-e2e-model',
            priority: 0,
            weight: 1,
            timeout_ms: 2000
          }
        ]
      },
      idempotency: 'catalog-browser-draft'
    }
  );
  expect(draft.status).toBe(201);
  expect(
    (
      await manage(
        page,
        'POST',
        `/api/v1/route-drafts/${draft.body.id}/activate`,
        {
          match: `/api/v1/route-drafts/${draft.body.id}`,
          idempotency: 'catalog-browser-route'
        }
      )
    ).status
  ).toBe(200);
  await expect
    .poll(
      async () => {
        const result = await manage<{ items: Array<{ id: string }> }>(
          page,
          'GET',
          '/api/v1/catalog'
        );
        return result.body.items.some((item) => item.id === 'catalog-browser');
      },
      { timeout: 15000 }
    )
    .toBe(true);
  await page.goto('/catalog');
  await expect(
    page.getByRole('heading', { name: 'Model catalog', exact: true })
  ).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'catalog-browser', exact: true })
  ).toBeVisible();
  const card = page.locator('article').filter({
    has: page.getByRole('heading', { name: 'catalog-browser', exact: true })
  });
  await expect(card).not.toContainText('compatible-e2e-model');
  await card.locator('summary').filter({ hasText: 'openai' }).click();
  await expect(card.locator('code').first()).toContainText('OLP_API_KEY');
  await card.getByRole('button', { name: 'Copy openai example' }).focus();
  await page.keyboard.press('Tab');
  await expect(
    card.getByRole('region', {
      name: 'catalog-browser: openai generation SDK example'
    })
  ).toBeFocused();
  await page.getByText('Catalog disclosure settings', { exact: true }).click();
  await page
    .getByLabel('Published route', { exact: true })
    .selectOption({ label: 'catalog-browser' });
  await page
    .getByLabel(
      'Disclose upstream model names in authenticated and published catalogs'
    )
    .check();
  await page.getByRole('button', { name: 'Save disclosure' }).click();
  await expect(page.getByText('Catalog disclosure saved.')).toBeVisible();
  await expect(card).toContainText('compatible-e2e-model');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByText('Catalog disclosure settings', { exact: true }).click();
  await page
    .getByLabel('Find a route', { exact: true })
    .fill('catalog-browser');
  await page.screenshot({
    path: info.outputPath('model-catalog.png'),
    fullPage: true
  });
  await page.goto('/project-policies');
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await page.getByLabel('Publish project catalog').check();
  await page.getByRole('button', { name: 'Save publication' }).click();
  await expect(page.getByText('Catalog publication saved.')).toBeVisible();
  const publicCatalog = await page.request.get(
    `/api/v1/catalog/public/${project.body.id}`
  );
  expect(publicCatalog.status()).toBe(200);
  expect((await publicCatalog.json()).prices_visible).toBe(false);
  const anonymous = await page
    .context()
    .browser()!
    .newContext({ baseURL: info.project.use.baseURL, reducedMotion: 'reduce' });
  try {
    const publicPage = await anonymous.newPage();
    await publicPage.goto(`/catalog/public/${project.body.id}`);
    await expect(
      publicPage.getByRole('heading', { name: 'catalog-browser', exact: true })
    ).toBeVisible();
    await expect(
      publicPage.getByText('Prices are not published for this catalog.')
    ).toBeVisible();
    await expect(
      publicPage.getByText('Catalog disclosure settings', { exact: true })
    ).toHaveCount(0);
    expect(
      (await new AxeBuilder({ page: publicPage }).analyze()).violations
    ).toEqual([]);
  } finally {
    await anonymous.close();
  }
});
