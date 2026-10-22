import { randomUUID } from 'node:crypto';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';
import type { components } from '../../src/lib/api/schema';

type Publication = components['schemas']['AgentPublication'];
type Summary = components['schemas']['AgentPublicationSummary'];
type Revision = components['schemas']['AgentPublicationRevision'];

test('publishes configuration, preserves historical prompt labels and retires without execution', async ({
  page
}) => {
  await signInGatewayOwner(page);
  const suffix = randomUUID().slice(0, 8);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    { body: { name: `Agent foundations ${suffix}` }, idempotency: randomUUID() }
  );
  expect(project.status).toBe(201);
  const consumerPaths: string[] = [];
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith('/v1/') || path.startsWith('/native/'))
      consumerPaths.push(path);
  });
  await page.goto('/agents');
  await expect(
    page.getByRole('heading', { name: 'Agent publications', exact: true })
  ).toBeVisible();
  await page.getByRole('button', { name: 'prompts', exact: true }).click();
  await page.getByRole('button', { name: 'New prompt', exact: true }).click();
  const name = `Greeting ${suffix}`;
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await page.getByLabel('Template', { exact: true }).fill('Hello {{name}}');
  await page.getByLabel('String variables (comma separated)').fill('name');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await expect(page.getByRole('status')).toHaveText(
    'Draft saved. Publication is a separate action.'
  );
  await page
    .getByRole('button', { name: 'Publish saved draft', exact: true })
    .click();
  await expect(page.getByLabel('Revision UUID')).not.toHaveValue('');
  const revision = await page.getByLabel('Revision UUID').inputValue();
  const list = await manage<{ items: Summary[] }>(
    page,
    'GET',
    '/api/v1/prompts?limit=200'
  );
  const publication = list.body.items.find((item) => item.name === name);
  expect(publication).toBeDefined();
  const path = `/api/v1/prompts/${publication!.id}`;
  await page.getByLabel('Template', { exact: true }).fill('Unsaved {{name}}');
  await expect(
    page.getByRole('button', { name: 'Publish saved draft', exact: true })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Set staging', exact: true })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Set production', exact: true })
  ).toBeDisabled();
  await expect(page.getByLabel('Template', { exact: true })).toHaveValue(
    'Unsaved {{name}}'
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await expect(page.getByRole('status')).toHaveText(
    'Draft saved. Publication is a separate action.'
  );
  await expect(page.getByLabel('Revision UUID')).toHaveValue(revision);
  await page
    .getByRole('button', { name: 'Set production', exact: true })
    .click();
  await expect(page.getByRole('status')).toHaveText(
    'Publication configuration updated.'
  );
  const current = await manage<Publication>(page, 'GET', path);
  const immutable = await manage<Revision>(
    page,
    'GET',
    `${path}/revisions/${revision}`
  );
  expect(current.body.draft.prompt?.template).toBe('Unsaved {{name}}');
  expect(current.body.revision_id).toBe(revision);
  expect(immutable.body.definition.prompt?.template).toBe('Hello {{name}}');
  expect(immutable.body.labels).toEqual(['production']);
  await page
    .getByRole('button', { name: 'Inspect immutable revision', exact: true })
    .click();
  await expect(page.locator('pre')).toContainText('Hello {{name}}');
  await page.getByLabel('Retire and revoke this publication').check();
  await page
    .getByRole('button', { name: 'Confirm retirement', exact: true })
    .click();
  await expect(page.getByRole('row').filter({ hasText: name })).toHaveCount(0);
  await page.goto('/execution-debits');
  await expect(
    page.getByRole('heading', { name: 'Provisional exposure', exact: true })
  ).toBeVisible();
  await page.getByLabel('API key UUID (optional)').fill(randomUUID());
  await page.getByLabel('API key UUID (optional)').press('Enter');
  await expect(page.getByText('No execution debit records.')).toBeVisible();
  expect(consumerPaths).toEqual([]);
});
