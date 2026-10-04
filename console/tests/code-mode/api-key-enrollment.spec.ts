import AxeBuilder from '@axe-core/playwright';
import { execFileSync } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';

// A fictional key: enrollment stores it encrypted and never calls Z.ai.
const pasted = 'fixture0123456789abcdef.CONTROLLEDbrowserKEY';
let module = '';
let digest = '';

test.beforeAll(() => {
  module = join(mkdtempSync(join(tmpdir(), 'olp-plugin-')), 'zai.wasm');
  execFileSync(
    'go',
    ['build', '-buildmode=c-shared', '-o', module, './plugins/zai-coding'],
    {
      cwd: fileURLToPath(new URL('../../..', import.meta.url)),
      env: { ...process.env, GOOS: 'wasip1', GOARCH: 'wasm', CGO_ENABLED: '0' },
      stdio: 'inherit'
    }
  );
  digest = createHash('sha256').update(readFileSync(module)).digest('hex');
});

test('an operator enrolls a pasted GLM Coding Plan key through a masked field', async ({
  page
}, info) => {
  test.setTimeout(180_000);
  await signInGatewayOwner(page);
  await page.goto('/plugins');
  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    'Installed zai-coding 0.1.0.'
  );
  const plugin = page.getByRole('article', { name: 'zai-coding 0.1.0' });
  await plugin.getByRole('button', { name: 'Review and approve' }).click();
  await plugin
    .getByRole('region', { name: 'Approve these origins?' })
    .getByRole('button', { name: 'Approve origins' })
    .click();
  await expect(page.getByRole('status')).toContainText(
    'Approved zai-coding 0.1.0.'
  );
  const suffix = randomUUID().slice(0, 8);
  const project = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/projects',
    { body: { name: `Coding plan ${suffix}` }, idempotency: randomUUID() }
  );
  const provider = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      body: {
        name: `GLM ${suffix}`,
        project_id: project.body.id,
        model: 'glm-5.3',
        configuration: {
          kind: 'plugin',
          auth_mode: 'grant',
          profile_id: 'zai-coding-plan',
          profile_revision: digest
        }
      },
      idempotency: randomUUID()
    }
  );
  expect(provider.status).toBe(201);

  await page.goto('/code-mode');
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(project.body.id);
  await page.getByRole('button', { name: 'Create account' }).click();
  await page
    .getByLabel('Provider', { exact: true })
    .selectOption(provider.body.id);
  await page
    .getByRole('button', { name: 'Enroll subscription account' })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Issue an upstream API key' })
  ).toBeVisible();
  await expect(
    page.getByRole('link', { name: 'Open API key page' })
  ).toHaveAttribute('href', 'https://z.ai/manage-apikey/apikey-list');
  const key = page.getByLabel('Paste the API key the vendor issued');
  await expect(key).toHaveAttribute('type', 'password');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('code-key-enrollment.png'),
    fullPage: true
  });
  await key.fill(pasted);
  await page.getByRole('button', { name: 'Continue', exact: true }).click();
  await expect(page.getByText(/Key enrolled\./)).toBeVisible();
  await page.getByLabel('Name', { exact: true }).fill(`GLM ${suffix}`);
  await page.getByLabel('Native models').fill('glm-5.3');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(
    page.getByText('GLM Coding Plan', { exact: true })
  ).toBeVisible();
  await expect(page.locator('body')).not.toContainText(pasted);

  const accounts = await manage<{ items: Array<Record<string, unknown>> }>(
    page,
    'GET',
    `/api/v1/code/accounts?project_id=${project.body.id}`
  );
  expect(accounts.source).not.toContain(pasted);
  expect(accounts.body.items[0]).toMatchObject({
    adapter: 'zai_coding',
    grant_state: 'current',
    health: 'unknown'
  });
  expect(String(accounts.body.items[0].principal)).toMatch(
    /^zai-coding-plan:[0-9a-f]{64}$/
  );
});
