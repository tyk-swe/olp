import { join } from 'node:path';
import { test as base, type Locator, type Page } from '@playwright/test';

export { expect } from '@playwright/test';

export const session = join(
  import.meta.dirname,
  '../../test-results/session.json'
);

export const owner = {
  name: 'Olive Owner',
  email: 'owner@example.test',
  password: 'a long browser test password'
};

export const route = 'browser-route';

type Vendor = {
  endpoint: string;
  secret: string;
  model: string;
  answer: string;
  inputTokens: number;
  outputTokens: number;
};

export const test = base.extend<object, { vendor: Vendor }>({
  vendor: [
    async ({ playwright }, use) => {
      const origin = process.env.OLP_E2E_VENDOR;
      const api = await playwright.request.newContext();
      const scripted = await api.get(`${origin}/fixture`);
      await use({ ...(await scripted.json()), endpoint: `${origin}/v1` });
      await api.dispose();
    },
    { scope: 'worker' }
  ]
});

export async function askPlayground(
  page: Page,
  streamed: boolean
): Promise<Locator> {
  await page.goto('/playground');
  await page.getByRole('combobox', { name: 'Route slug' }).fill(route);
  await page.getByRole('textbox', { name: 'Prompt' }).fill('hello');
  await page
    .getByRole('checkbox', { name: 'Stream the response' })
    .setChecked(streamed);
  await page.getByRole('button', { name: 'Run test' }).click();
  return page.getByRole('region', { name: 'Result' });
}
