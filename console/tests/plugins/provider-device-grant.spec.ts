import { expect, test } from '../playwright';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import {
  approveReferencePlugin,
  buildReferencePlugin,
  installReferencePlugin
} from './referencePlugin';

// tests/plugins/mock-plugin-upstream.mjs: the upstream, and under /oauth the
// fake authority the reference plugin enrolls grants with, including by
// device authorization.
const upstream = {
  origin: 'http://127.0.0.1:4190',
  address: 'http://127.0.0.1:4190/v1',
  authority: 'http://127.0.0.1:4190/oauth',
  model: 'reference-e2e-model',
  account: 'acct-e2e'
};
const version = '0.4.0';

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
  ({ module, digest } = buildReferencePlugin({
    upstream: upstream.address,
    authority: upstream.authority,
    version
  }));
});

test('an operator connects a provider by approving a device code upstream', async ({
  page,
  request
}, info) => {
  test.setTimeout(180_000);
  await signIn(page);
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);

  const plugin = await installReferencePlugin(page, module, version);
  await approveReferencePlugin(page, plugin, version);

  await page.goto('/providers/new');
  await page.getByRole('radio', { name: /Provider plugin/ }).check();
  await page
    .getByLabel('Plugin profile')
    .selectOption(`reference-device-chat@${digest}`);
  await expect(page.getByLabel('Authentication', { exact: true })).toHaveValue(
    'grant'
  );
  await page.getByLabel('Provider name').fill('Reference device account');
  await page.getByLabel('Probe model').fill(upstream.model);
  await page.getByRole('button', { name: /Save and sign in upstream/ }).click();

  // OLP shows the verification URL and the user code, each with a copy
  // action, while it waits for the approval.
  const enrollment = page.getByRole('region', { name: 'Sign in upstream' });
  const verification = enrollment.getByRole('link', {
    name: 'Open verification page'
  });
  await expect(verification).toHaveAttribute(
    'href',
    `${upstream.authority}/device`
  );
  const userCode = (await enrollment.locator('.user-code').textContent())!;
  expect(userCode).toMatch(/^[0-9A-F]{4}-[0-9A-F]{4}$/);
  await expect(enrollment.getByRole('status')).toContainText(
    'Waiting for approval upstream'
  );
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  for (const [label, value] of [
    ['Copy verification URL', `${upstream.authority}/device`],
    ['Copy user code', userCode]
  ]) {
    await enrollment.getByRole('button', { name: label }).click();
    await expect(enrollment.getByRole('button', { name: label })).toHaveText(
      'Copied'
    );
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
      value
    );
  }
  await page.screenshot({
    path: info.outputPath('provider-device-grant-enrollment.png'),
    fullPage: true
  });

  // The operator approves the device on the authority's verification page,
  // and the console, polling the enrollment's status, continues to models.
  const upstreamTab = await page.context().newPage();
  await upstreamTab.goto(`${upstream.authority}/device`);
  await upstreamTab.getByLabel('User code').fill(userCode);
  await upstreamTab.getByRole('button', { name: 'Approve' }).click();
  await expect(upstreamTab.getByText('Device approved.')).toBeVisible();
  await upstreamTab.close();

  await expect(
    page.getByRole('heading', { name: 'Declare upstream models' })
  ).toBeVisible();
  await expect(enrollment).toHaveCount(0);

  // Testing the connection carried the grant's access token and the account
  // the grant authorizes.
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
