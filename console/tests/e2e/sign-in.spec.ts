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

  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true })
  ).toBeVisible();
  await expect(page.getByRole('link', { name: owner.name })).toBeVisible();
});

test('a bookmark opens the destination overview behind its own login', async ({
  page
}) => {
  await page.goto('/keys');
  await signIn(page);
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true })
  ).toBeVisible();

  const { port } = new URL(page.url());
  const destination = new URL(`http://localhost:${port}`);
  const sidebar = page.getByRole('complementary');
  await sidebar.getByLabel(/Choose installation/).click();
  await sidebar.getByLabel('Bookmark name').fill('Destination');
  await sidebar.getByLabel('Installation origin').fill(destination.origin);
  await sidebar.getByRole('button', { name: 'Add bookmark' }).click();
  await sidebar
    .getByRole('combobox', { name: 'Switch installation' })
    .selectOption(destination.origin);

  await expect(page).toHaveURL(
    new URL('/login?return_to=%2F', destination).href
  );
  await expect(
    page.getByRole('button', { name: 'Sign in', exact: true })
  ).toBeVisible();
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
