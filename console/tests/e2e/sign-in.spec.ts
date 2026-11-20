import { expect, owner, test } from './journey';

test.use({ storageState: { cookies: [], origins: [] } });

async function signIn(page: import('@playwright/test').Page) {
  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

test('a signed-out visitor reaches the console only after signing in', async ({
  page
}) => {
  await page.goto('/keys');
  await expect(page).toHaveURL(/\/login/);

  await signIn(page);

  await expect(page.getByRole('heading', { name: 'API keys' })).toBeVisible();
  await expect(page.getByRole('link', { name: owner.name })).toBeVisible();
});

test('installation branding restyles the sign-in page for signed-out visitors', async ({
  page,
  browser
}) => {
  await page.goto('/settings/general');
  await signIn(page);
  await page.getByLabel('Installation name').fill('Harbor Proxy');
  await page.getByLabel('Brand color hex').fill('#0f766e');
  await page.getByRole('button', { name: 'Save identity' }).click();
  await expect(page.getByText('Installation branding saved.')).toBeVisible();
  await page.mouse.move(0, 0);
  await expect(page.getByRole('button', { name: 'Save identity' })).toHaveCSS(
    'background-color',
    'rgb(15, 118, 110)'
  );

  const visitor = await browser.newPage({
    storageState: { cookies: [], origins: [] }
  });
  await visitor.goto('/login');
  await expect(
    visitor.getByText('Sign in to manage Harbor Proxy.')
  ).toBeVisible();
  await expect(visitor).toHaveTitle(/· Harbor Proxy$/);
  await expect(
    visitor.getByRole('button', { name: 'Sign in', exact: true })
  ).toHaveCSS('background-color', 'rgb(15, 118, 110)');
  await visitor.close();
});
