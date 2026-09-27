import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from '../playwright';
import { signInGatewayOwner as signIn } from '../gateway/signIn';

// tests/plugins/mock-plugin-upstream.mjs: the upstream, and under /oauth the
// fake authority the reference plugin enrolls grants with.
const upstream = {
  origin: 'http://127.0.0.1:4190',
  address: 'http://127.0.0.1:4190/v1',
  authority: 'http://127.0.0.1:4190/oauth',
  model: 'reference-e2e-model',
  subject: 'operator@reference.example',
  account: 'acct-e2e'
};
// The reference plugin sends the operator's browser here after sign-in.
const loopback = 'http://127.0.0.1:1455/callback';
const version = '0.3.0';
const providerName = 'Reference account';

type Recorded = {
  requests: {
    path: string;
    headers: Record<string, string>;
  }[];
  unexpected: string[];
};

let module = '';
let digest = '';

test.beforeAll(() => {
  module = join(mkdtempSync(join(tmpdir(), 'olp-plugin-')), 'reference.wasm');
  execFileSync(
    'go',
    [
      'build',
      '-buildmode=c-shared',
      `-ldflags=-X=main.upstream=${upstream.address} -X=main.authority=${upstream.authority} -X=main.version=${version}`,
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

test('an operator connects a provider by signing in to an upstream account', async ({
  page,
  request
}, info) => {
  test.setTimeout(180_000);
  await signIn(page);
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);

  // An owner installs the plugin and approves its origin, where both its
  // upstream and its authority are.
  await page.goto('/plugins');
  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    `Installed reference ${version}.`
  );
  const plugin = page.getByRole('article', { name: `reference ${version}` });
  await expect(
    plugin.getByRole('row', { name: /reference-grant-chat/ })
  ).toContainText('Grant, enrolled by the plugin');
  await plugin.getByRole('button', { name: 'Review and approve' }).click();
  await plugin
    .getByRole('region', { name: 'Approve these origins?' })
    .getByRole('button', { name: 'Approve origins' })
    .click();
  await expect(page.getByRole('status')).toContainText(
    `Approved reference ${version}.`
  );

  // The grant profile replaces the credential with grant enrollment.
  await page.goto('/providers/new');
  await page.getByRole('radio', { name: /Provider plugin/ }).check();
  await page
    .getByLabel('Plugin profile')
    .selectOption(`reference-grant-chat@${digest}`);
  await expect(page.getByLabel('Authentication', { exact: true })).toHaveValue(
    'grant'
  );
  await expect(page.getByLabel('Credential', { exact: true })).toHaveCount(0);
  await page.getByLabel('Provider name').fill(providerName);
  await page.getByLabel('Probe model').fill(upstream.model);
  await page.getByRole('button', { name: /Save and sign in upstream/ }).click();

  // OLP shows the plugin's authorization URL, which the operator copies and
  // opens to sign in upstream.
  const enrollment = page.getByRole('region', { name: 'Sign in upstream' });
  const authorization = enrollment.getByRole('link', {
    name: 'Open authorization page'
  });
  await expect(authorization).toHaveAttribute(
    'href',
    new RegExp(`^${upstream.authority}/authorize\\?`)
  );
  const authorizationURL = (await authorization.getAttribute('href'))!;
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await enrollment
    .getByRole('button', { name: 'Copy authorization URL' })
    .click();
  await expect(
    enrollment.getByRole('button', { name: 'Copy authorization URL' })
  ).toHaveText('Copied');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    authorizationURL
  );
  await page.screenshot({
    path: info.outputPath('provider-grant-enrollment.png'),
    fullPage: true
  });

  // The authority returns the browser to a loopback address where nothing
  // listens; the operator copies the callback URL from the address bar.
  const upstreamTab = await page.context().newPage();
  const returned = upstreamTab.waitForRequest((sent) =>
    sent.url().startsWith(`${loopback}?`)
  );
  await upstreamTab.goto(authorizationURL).catch(() => undefined);
  const callback = (await returned).url();
  await upstreamTab.close();

  await enrollment
    .getByLabel('Paste back the callback URL or code the upstream returned')
    .fill(callback);
  await enrollment.getByRole('button', { name: 'Continue' }).click();

  // The grant is a credential version; the wizard tests the connection with
  // it and moves on to models.
  await expect(
    page.getByRole('heading', { name: 'Declare upstream models' })
  ).toBeVisible();
  await expect(
    page.getByText(new URL(callback).searchParams.get('code')!)
  ).toHaveCount(0);
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

  // Every upstream call carried the grant's access token and the account
  // the grant authorizes, a grant fact.
  const observed = (await (
    await request.get(`${upstream.origin}/__test__/requests`)
  ).json()) as Recorded;
  expect(observed.unexpected).toEqual([]);
  expect(observed.requests.length).toBeGreaterThan(0);
  for (const call of observed.requests) {
    expect(call.headers.authorization).toMatch(/^Bearer /);
    expect(call.headers['x-reference-account']).toBe(upstream.account);
  }
});
