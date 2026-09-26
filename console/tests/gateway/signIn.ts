import { readFileSync } from 'node:fs';
import { expect, type Page } from '../playwright';

const owner = {
  email: 'owner@example.com',
  password: 'a long browser test password'
};

export async function signInGatewayOwner(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (
    await page.getByRole('button', { name: 'Create owner account' }).isVisible()
  ) {
    await page.getByLabel('Display name').fill('Owner');
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
    await page.getByRole('button', { name: 'Create owner account' }).click();
  } else {
    const deadline = Date.now() + 75_000;
    while (true) {
      await page.getByLabel('Email').fill(owner.email);
      await page.getByLabel('Password', { exact: true }).fill(owner.password);
      const completed = page.waitForResponse(
        (response) =>
          response.request().method() === 'POST' &&
          new URL(response.url()).pathname === '/api/v1/sessions'
      );
      await page.getByRole('button', { name: 'Sign in' }).click();
      const response = await completed;
      if (response.status() === 201) break;
      if (response.status() !== 429 || Date.now() >= deadline) {
        expect(response.status()).toBe(201);
        break;
      }
      const requested = Number(response.headers()['retry-after'] ?? '1');
      const seconds = Number.isFinite(requested)
        ? Math.min(60, Math.max(1, requested))
        : 1;
      await new Promise((resolve) => setTimeout(resolve, seconds * 1000));
    }
  }
  await expect(page).toHaveURL(/\/$/);
}
