import { signInOwner } from '../helpers/owner';
import { expect, type Page, type Locator } from '../playwright';

export const owner = {
  name: 'Integration Owner',
  email: 'console-integration@example.com',
  password: 'correct horse battery staple'
};

export const vertical = {
  providerName: 'Vertical Azure provider',
  deployment: 'vertical-e2e-deployment',
  endpoint: 'http://127.0.0.1:4178',
  apiVersion: '2024-10-21',
  credential: 'vertical-provider-secret',
  route: 'vertical-all-protocols',
  keyName: 'Vertical all-protocol key',
  reply: 'Hello from the vertical upstream'
};

export function signInAsOwner(page: Page): Promise<void> {
  return signInOwner(page, owner, {
    bootstrapTokenFile: process.env.OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE
  });
}

/// Reads the one-time secret out of the reveal dialog.
export async function takeSecret(dialog: Locator): Promise<string> {
  const secret = (
    await dialog.locator('.secret-row code').textContent()
  )?.trim();
  return secret ?? '';
}

export async function waitForRoutePublication(
  page: Page,
  secret: string,
  route: string
): Promise<void> {
  await expect
    .poll(
      async () =>
        page.evaluate(
          async ({ apiKey, routeSlug }) => {
            try {
              const response = await fetch('/v1/models', {
                headers: { Authorization: `Bearer ${apiKey}` }
              });
              if (!response.ok) {
                await response.body?.cancel();
                return false;
              }
              const payload = (await response.json()) as {
                data?: Array<{ id?: string }>;
              };
              return (
                payload.data?.some((model) => model.id === routeSlug) ?? false
              );
            } catch {
              return false;
            }
          },
          { apiKey: secret, routeSlug: route }
        ),
      {
        message: `route ${route} and its API key should publish to the runtime`,
        timeout: 30_000,
        intervals: [250, 500, 1_000]
      }
    )
    .toBe(true);
}
