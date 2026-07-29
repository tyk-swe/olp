import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Locator, type Page } from '@playwright/test';

// The `zz-` prefix is load-bearing. Playwright runs spec files in path order,
// this project runs one worker against one real database, and
// `rust-hosted-console.spec.ts` owns first-run setup — it asserts that `/`
// redirects to `/setup`, which is true exactly once per installation. This
// file signs in as the owner that spec creates, so it has to run after it.
const owner = {
  email: 'console-integration@example.com',
  password: 'correct horse battery staple'
};

/// Reads the one-time secret out of the reveal dialog.
async function takeSecret(dialog: Locator): Promise<string> {
  const secret = (await dialog.locator('.secret-value, code, pre').first().textContent())?.trim();
  return secret ?? '';
}

/// Asserts a secret is nowhere in the rendered page.
///
/// Compared as a boolean rather than with `toContain`, so a failure reports the
/// claim instead of dumping the entire document into the run log.
async function expectSecretGone(
  page: Page,
  secret: string,
  what: string
): Promise<void> {
  const html = await page.content();
  expect(html.includes(secret), `${what} must not be retrievable after it is dismissed`).toBe(
    false
  );
}

test('API key secrets are shown once and the lifecycle converges against the real backend', async ({
  page,
  context
}) => {
  page.on('dialog', (dialog) => dialog.accept());

  await page.goto('/login');
  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password').fill(owner.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/$/);

  // The mocked specs install their own cookie, so only this tier can show that
  // the server issues the `__Host-` contract the console depends on.
  const session = (await context.cookies(new URL(page.url()).origin)).filter((cookie) =>
    cookie.name.startsWith('__Host-')
  );
  expect(session.length).toBeGreaterThan(0);
  for (const cookie of session) {
    expect(cookie.path).toBe('/');
    expect(cookie.secure).toBe(true);
  }

  const keyName = `integration key ${Date.now()}`;
  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill(keyName);
  await page.getByLabel('Requests per minute').fill('120');
  await page.getByLabel('Concurrent requests').fill('8');
  await page.getByRole('button', { name: /Create and show key/ }).click();

  const created = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(created).toBeVisible();
  const secret = await takeSecret(created);
  // `CreateApiKeyResponse.secret` is documented "Returned only by this creation
  // response", so the value has to be real and then has to disappear.
  expect(secret).toMatch(/^olp_/);
  expect((await new AxeBuilder({ page }).include('.secret-dialog').analyze()).violations).toEqual(
    []
  );
  await created.getByRole('button', { name: 'I have saved the key' }).click();

  await expect(page).toHaveURL(/\/api-keys$/);
  await expect(page.getByText(keyName)).toBeVisible();
  await expect(page.getByText(secret)).toHaveCount(0);

  // A reload goes back to the server: if the secret came back on a listing,
  // "returned only by this creation response" would be false.
  await page.reload();
  await expect(page.getByText(keyName)).toBeVisible();
  await expectSecretGone(page, secret, 'the created secret');

  const row = page.getByRole('row').filter({ hasText: keyName });
  await expect(row).toHaveCount(1);
  await row.getByRole('button', { name: 'Rotate' }).click();
  const rotated = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(rotated).toBeVisible();
  const rotatedSecret = await takeSecret(rotated);
  expect(rotatedSecret).toMatch(/^olp_/);
  // A rotation that returned the same material would not be a rotation.
  expect(rotatedSecret).not.toBe(secret);
  await rotated.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(rotated).toBeHidden();
  await page.reload();
  await expectSecretGone(page, rotatedSecret, 'the rotated secret');

  await row.getByRole('button', { name: 'Revoke' }).click();
  await expect(row.getByText('revoked', { exact: true })).toBeVisible();

  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
