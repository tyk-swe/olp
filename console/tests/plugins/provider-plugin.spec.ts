import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from '../playwright';
import { signInGatewayOwner as signIn } from '../gateway/signIn';

// tests/plugins/mock-plugin-upstream.mjs, which accepts only the headers the
// reference plugin's hosting adaptation declares.
const upstream = {
  origin: 'http://127.0.0.1:4190',
  address: 'http://127.0.0.1:4190/v1',
  model: 'reference-e2e-model',
  credential: 'reference-plugin-secret'
};
const version = '0.2.0';
const providerName = 'Reference plugin upstream';

type Recorded = {
  requests: {
    method: string;
    path: string;
    headers: Record<string, string>;
    body: { stream?: boolean } | null;
  }[];
  unexpected: string[];
};

let module = '';
let digest = '';

// The reference plugin, linked against the fake upstream and labelled apart
// from the default build the install journey uses.
test.beforeAll(() => {
  module = join(mkdtempSync(join(tmpdir(), 'olp-plugin-')), 'reference.wasm');
  execFileSync(
    'go',
    [
      'build',
      '-buildmode=c-shared',
      `-ldflags=-X=main.upstream=${upstream.address} -X=main.version=${version}`,
      '-o',
      module,
      './sdk/plugin/reference'
    ],
    {
      cwd: fileURLToPath(new URL('../../..', import.meta.url)),
      env: { ...process.env, GOOS: 'wasip1', GOARCH: 'wasm', CGO_ENABLED: '0' },
      stdio: 'inherit'
    }
  );
  digest = createHash('sha256').update(readFileSync(module)).digest('hex');
});

test('an operator connects a provider through an approved plugin profile', async ({
  page,
  request
}, info) => {
  test.setTimeout(180_000);
  await signIn(page);
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);

  // An owner installs the plugin and approves the origins it declares,
  // including the one its profile's address uses.
  await page.goto('/plugins');
  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    `Installed reference ${version}.`
  );
  const plugin = page.getByRole('article', { name: `reference ${version}` });
  await expect(
    plugin.getByRole('cell', { name: upstream.address, exact: true })
  ).toBeVisible();
  await plugin.getByRole('button', { name: 'Review and approve' }).click();
  await plugin
    .getByRole('region', { name: 'Approve these origins?' })
    .getByRole('button', { name: 'Approve origins' })
    .click();
  await expect(page.getByRole('status')).toContainText(
    `Approved reference ${version}.`
  );

  // The provider wizard offers the plugin's profile and digest, then the
  // static credential. The profile's address is the endpoint.
  await page.goto('/providers/new');
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('radio', { name: /Provider plugin/ }).check();
  const profile = page.getByLabel('Plugin profile');
  await expect(
    profile.locator(`option[value="reference-chat@${digest}"]`)
  ).toHaveText(
    `Reference Chat Completions · reference ${version} · digest ${digest.slice(0, 12)}`
  );
  await profile.selectOption(`reference-chat@${digest}`);
  const pinned = page.getByRole('region', { name: 'Pinned plugin' });
  await expect(pinned).toContainText(`reference ${version}`);
  await expect(pinned).toContainText(digest);
  await expect(page.getByRole('textbox', { name: 'Endpoint' })).toHaveCount(0);
  await page.getByLabel('Provider name').fill(providerName);
  await page.getByLabel('Probe model').fill(upstream.model);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(upstream.credential);
  await page.screenshot({
    path: info.outputPath('provider-plugin-wizard.png'),
    fullPage: true
  });
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/api/v1/providers'
  );
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const providerId = ((await (await created).json()) as { id: string }).id;
  await expect(page.getByText(upstream.credential)).toHaveCount(0);

  // Plugin providers have no model list: the probe model is declared, and
  // each model is certified.
  await expect(
    page.getByRole('heading', { name: 'Declare upstream models' })
  ).toBeVisible();
  await expect(
    page.getByText(upstream.model, { exact: true }).first()
  ).toBeVisible();
  await page.getByLabel('Operation 1').selectOption('generation');
  await page.getByLabel('Client surface 1').selectOption('openai');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByLabel('Operation 2').selectOption('generation');
  await page.getByLabel('Client surface 2').selectOption('openai');
  await page.getByLabel('Mode 2').selectOption('streaming');
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();

  // The provider detail shows the plugin build it pins.
  await page.goto(`/providers/${providerId}`);
  const detail = page.getByRole('region', { name: 'Pinned plugin' });
  await expect(detail).toContainText(`reference ${version}`);
  await expect(detail).toContainText('Reference Chat Completions');
  await expect(detail).toContainText(digest);
  await expect(detail).toContainText(upstream.address);

  // Every upstream call carried the headers the hosting adaptation declares,
  // filled from the static credential.
  const observed = (await (
    await request.get(`${upstream.origin}/__test__/requests`)
  ).json()) as Recorded;
  expect(observed.unexpected).toEqual([]);
  expect(
    observed.requests.some(
      (call) =>
        call.path === '/v1/chat/completions' && call.body?.stream === true
    )
  ).toBe(true);
  for (const call of observed.requests) {
    expect(call.headers.authorization).toBe(`Token ${upstream.credential}`);
    expect(call.headers['x-reference-client']).toBe('olp');
  }

  // A plugin a provider revision pins can't be uninstalled.
  await page.goto('/plugins');
  page.once('dialog', (dialog) => dialog.accept());
  await plugin.getByRole('button', { name: 'Uninstall' }).click();
  await expect(page.getByRole('alert')).toContainText(providerName);
  await expect(plugin).toHaveCount(1);
});
