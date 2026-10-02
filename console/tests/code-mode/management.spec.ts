import AxeBuilder from '@axe-core/playwright';
import { randomUUID } from 'node:crypto';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';
import type {
  CodeBudget,
  CodePool,
  CodeRoute
} from '../../src/lib/api/code-mode';

test('manages real project pools, draft routes, publications and overlapping token budgets without inference', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const suffix = randomUUID().slice(0, 8);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    { body: { name: `Code mode ${suffix}` }, idempotency: randomUUID() }
  );
  expect(project.status).toBe(201);
  await page.goto('/code-mode');
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await expect(page.getByText('No accounts in this project.')).toBeVisible();
  await page.getByRole('button', { name: 'Pools', exact: true }).click();
  await page.getByRole('button', { name: 'Create pool', exact: true }).click();
  await page.getByLabel('Name', { exact: true }).fill(`Shared ${suffix}`);
  await page.getByLabel('Pool kind').selectOption('shared');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('None — no key can use this pool')).toBeVisible();
  const pools = await manage<{ items: CodePool[] }>(
    page,
    'GET',
    `/api/v1/code/pools?project_id=${project.body.id}`
  );
  const pool = pools.body.items.find(
    (item) => item.name === `Shared ${suffix}`
  )!;
  expect(pool.account_ids).toEqual([]);
  expect(pool.api_key_ids).toEqual([]);
  await page.getByRole('button', { name: 'Routes', exact: true }).click();
  await page.getByRole('button', { name: 'Create route', exact: true }).click();
  await page.getByLabel('Route slug').fill(`code-${suffix}`);
  await page.getByLabel('Pool', { exact: true }).selectOption(pool.id);
  await page.getByLabel('Native models').fill('fixture-native-model');
  await page.getByLabel('Enabled in next publication').uncheck();
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Not published', { exact: true })).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  await page
    .getByRole('button', { name: 'Publish route', exact: true })
    .click();
  await expect(
    page.getByText('Route published without synthetic inference.', {
      exact: false
    })
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Revisions and client setup' })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Published route revisions' })
  ).toBeVisible();
  await expect(page.getByText('Revision 1', { exact: true })).toBeVisible();
  const routes = await manage<{ items: CodeRoute[] }>(
    page,
    'GET',
    `/api/v1/code/routes?project_id=${project.body.id}`
  );
  const route = routes.body.items[0];
  expect(route.models).toEqual(['fixture-native-model']);
  expect(route.enabled).toBe(false);
  expect(route.revision).toBe(1);
  await page
    .getByRole('button', { name: 'Token budgets', exact: true })
    .click();
  await page
    .getByRole('button', { name: 'Create budget', exact: true })
    .click();
  await page.getByLabel('Daily hard token limit (UTC)').fill('12000');
  await page.getByLabel('Monthly hard token limit (UTC)').fill('200000');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('12,000', { exact: true })).toBeVisible();
  await page
    .getByRole('button', { name: 'Create budget', exact: true })
    .click();
  await page.getByLabel('Route scope').selectOption(route.id);
  await page.getByLabel('Daily hard token limit (UTC)').fill('5000');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  const budgets = await manage<{ items: CodeBudget[] }>(
    page,
    'GET',
    `/api/v1/code/budgets?project_id=${project.body.id}`
  );
  expect(budgets.body.items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        route_id: null,
        daily_tokens: 12000,
        monthly_tokens: 200000
      }),
      expect.objectContaining({
        route_id: route.id,
        daily_tokens: 5000,
        monthly_tokens: null
      })
    ])
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('code-budgets-mobile.png'),
    fullPage: true
  });
});
