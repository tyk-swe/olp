import { expect, owner, test } from './journey';

test.use({ storageState: { cookies: [], origins: [] } });

test('a signed-out visitor reaches the console only after signing in', async ({
  page
}) => {
  await page.goto('/keys');
  await expect(page).toHaveURL(/\/login/);

  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();

  await expect(page.getByRole('heading', { name: 'API keys' })).toBeVisible();
  await expect(page.getByRole('link', { name: owner.name })).toBeVisible();
});
