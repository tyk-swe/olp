import { randomUUID } from 'node:crypto';
import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage, provisionGenerationRoute } from '../helpers/management';
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

test('renders published prompts in the playground after moving a label', async ({
  page,
  request
}, info) => {
  await signInGatewayOwner(page);
  const suffix = randomUUID().slice(0, 8);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    {
      body: { name: `Prompt rendering ${suffix}` },
      idempotency: randomUUID()
    }
  );
  expect(project.status).toBe(201);
  const slug = `prompt-render-${suffix}`;
  await provisionGenerationRoute(page, {
    providerName: `Prompt provider ${suffix}`,
    route: slug,
    endpoint: 'http://127.0.0.1:4187/v1',
    model: 'compatible-e2e-model',
    credential: 'compatible-provider-secret',
    projectId: project.body.id
  });
  await page.goto('/agents');
  await page.getByRole('button', { name: 'prompts', exact: true }).click();
  await page.getByRole('button', { name: 'New prompt', exact: true }).click();
  const name = `Playground prompt ${suffix}`;
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await page.getByLabel('Template', { exact: true }).fill('Hello {{name}}');
  await page.getByLabel('String variables (comma separated)').fill('name');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await page
    .getByRole('button', { name: 'Publish saved draft', exact: true })
    .click();
  await expect(page.getByLabel('Revision UUID')).not.toHaveValue('');
  await page
    .getByRole('button', { name: 'Set production', exact: true })
    .click();
  await expect(page.getByRole('status')).toHaveText(
    'Publication configuration updated.'
  );
  const list = await manage<{ items: Summary[] }>(
    page,
    'GET',
    '/api/v1/prompts?limit=200'
  );
  const publication = list.body.items.find((item) => item.name === name);
  expect(publication).toBeDefined();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  const reference = {
    olp_prompt: {
      id: publication!.id,
      label: 'production',
      variables: { name: 'Ada' }
    }
  };
  await expect
    .poll(
      async () =>
        (
          await manage(page, 'POST', '/api/v1/playground', {
            body: { model: slug, request: reference }
          })
        ).status,
      { timeout: 15_000 }
    )
    .toBe(200);
  await request.post('http://127.0.0.1:4187/__test__/reset');
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(slug);
  await page.getByRole('radio', { name: 'Advanced' }).check();
  await page.getByLabel('Request JSON').fill(JSON.stringify(reference));
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.locator('.output pre').first()).toHaveText(
    'Hello from the compatible upstream'
  );
  const first = await (
    await request.get('http://127.0.0.1:4187/__test__/requests')
  ).json();
  expect(first.requests.at(-1).body.messages).toEqual([
    { role: 'user', content: 'Hello Ada' }
  ]);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('prompt-playground-after.png'),
    fullPage: true
  });

  await page.goto('/agents');
  await page.getByRole('button', { name: 'prompts', exact: true }).click();
  await page
    .getByRole('row')
    .filter({ hasText: name })
    .getByRole('button', { name: 'Open' })
    .click();
  await page.getByLabel('Template', { exact: true }).fill('Welcome {{name}}');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await page
    .getByRole('button', { name: 'Publish saved draft', exact: true })
    .click();
  await page
    .getByRole('button', { name: 'Set production', exact: true })
    .click();
  await expect(page.getByRole('status')).toHaveText(
    'Publication configuration updated.'
  );
  await expect
    .poll(
      async () => {
        const result = await manage(page, 'POST', '/api/v1/playground', {
          body: { model: slug, request: reference }
        });
        const traffic = await (
          await request.get('http://127.0.0.1:4187/__test__/requests')
        ).json();
        return result.status === 200
          ? traffic.requests.at(-1).body.messages
          : null;
      },
      { timeout: 15_000 }
    )
    .toEqual([{ role: 'user', content: 'Welcome Ada' }]);
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(slug);
  await page.getByRole('radio', { name: 'Advanced' }).check();
  await page.getByLabel('Request JSON').fill(JSON.stringify(reference));
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.locator('.output pre').first()).toHaveText(
    'Hello from the compatible upstream'
  );
  const moved = await (
    await request.get('http://127.0.0.1:4187/__test__/requests')
  ).json();
  expect(moved.requests.at(-1).body.messages).toEqual([
    { role: 'user', content: 'Welcome Ada' }
  ]);
});
