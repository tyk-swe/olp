import { expect, test } from '@playwright/test';

test('console hydrates and uses the Go origin through packaged assets and Vite', async ({
  page,
  request
}, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (event) => {
      throw new Error(`CSP blocked ${event.blockedURI}`);
    });
  });
  const definition = await request.get('/api/v3/openapi.json');
  expect(definition.status()).toBe(200);
  expect((await definition.json()).openapi).toBe('3.1.0');
  const deferred = await request.get('/api/v3/sessions/current');
  expect(deferred.status()).toBe(401);
  expect((await deferred.json()).status).toBe(401);
  await page.goto('/login');
  await expect(page.locator('#svelte-root')).not.toBeEmpty();
  await expect(page.locator('body')).toContainText(
    /sign in|unable|failed|try again|error/i
  );
  expect(errors).toEqual([]);
  await page.screenshot({ path: testInfo.outputPath('console-shell.png') });
});
