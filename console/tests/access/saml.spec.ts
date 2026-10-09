import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';

test('imports SAML metadata and completes a cross-site signed browser login', async ({
  page,
  browser,
  baseURL
}, info) => {
  await signInGatewayOwner(page);
  await page.goto('/access');
  await page
    .getByRole('navigation', { name: 'Access settings' })
    .getByRole('button', { name: 'SAML', exact: true })
    .click();
  await page
    .getByLabel('Metadata URL', { exact: true })
    .fill('http://localhost:4196/metadata');
  await page
    .getByRole('button', { name: 'Import metadata', exact: true })
    .click();
  await expect(page.getByRole('status')).toContainText('Metadata imported');
  await page.getByLabel('Enable SAML sign-in').check();
  await page
    .getByLabel('Group role mappings (JSON)')
    .fill(JSON.stringify([{ claim_value: 'developers', role: 'developer' }]));
  await page.getByRole('button', { name: 'Save SAML configuration' }).click();
  await expect(page.getByRole('status')).toContainText(
    'SAML configuration saved'
  );
  await expect(page.getByLabel('Public signing certificate')).toHaveValue(
    /BEGIN CERTIFICATE/
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: info.outputPath('saml-configuration.png'),
    fullPage: true
  });
  const context = await browser.newContext({
    baseURL,
    reducedMotion: 'reduce'
  });
  try {
    const member = await context.newPage();
    await member.goto('/login');
    await member.getByRole('button', { name: 'Continue with SAML' }).click();
    await expect(member).toHaveURL(/^http:\/\/localhost:4196\/sign-in/);
    expect(
      (await context.request.get('/api/v1/sessions/current')).status()
    ).toBe(401);
    const received = member.waitForResponse(
      (r) =>
        r.url().endsWith('/api/v1/saml/acs') && r.request().method() === 'POST'
    );
    await member.getByRole('button', { name: 'Continue to console' }).click();
    const acs = await received;
    expect(acs.status()).toBe(303);
    expect(acs.headers()['set-cookie']).toBeUndefined();
    await expect(member).not.toHaveURL(
      /\/login|\/api\/v1\/saml|localhost:4196/
    );
    const profile = await member.evaluate(() =>
      fetch('/api/v1/profile').then((r) => r.json())
    );
    expect(profile.email).toBe('saml-browser@example.com');
    expect(profile.role).toBe('developer');
    await member.goto('/settings/profile');
    await expect(
      member.getByRole('region', { name: 'SAML identities' })
    ).toContainText('saml-browser@example.com');
    expect(
      (await new AxeBuilder({ page: member }).analyze()).violations
    ).toEqual([]);
  } finally {
    await context.close();
  }
});
