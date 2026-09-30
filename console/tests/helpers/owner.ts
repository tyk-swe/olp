import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Page } from '../playwright';

type Owner = { name: string; email: string; password: string };

// Each run owns its output directory. Cache only browser cookies, validate
// them against the real backend, and renew through the UI after revocation.
export async function signInOwner(
  page: Page,
  owner: Owner,
  {
    reuseSession = true,
    bootstrapTokenFile = process.env.OLP_BOOTSTRAP_TOKEN_FILE
  } = {}
): Promise<void> {
  const info = test.info();
  const origin = info.project.use.baseURL!;
  const key = createHash('sha256')
    .update(`${origin}/${owner.email}`)
    .digest('hex');
  const stateFile = join(info.project.outputDir, `.owner-${key}.json`);
  if (reuseSession && existsSync(stateFile)) {
    const state = JSON.parse(readFileSync(stateFile, 'utf8')) as Awaited<
      ReturnType<ReturnType<Page['context']>['storageState']>
    >;
    await page.context().addCookies(state.cookies);
    await page.goto('/');
    // Validate in Chromium, which applies the localhost Secure-cookie
    // exception. Playwright's API client follows stricter HTTP cookie rules.
    const response = await page.evaluate(async () => {
      const response = await fetch('/api/v1/sessions/current');
      return {
        status: response.status,
        email: response.ok ? (await response.json()).user.email : null
      };
    });
    if (response.status === 200 && response.email === owner.email) {
      await expect(page).toHaveURL(/\/$/);
      return;
    }
    expect(response.status).toBe(401);
    await page.context().clearCookies();
  }

  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (/\/setup$/.test(page.url())) {
    await page.getByLabel('Display name').fill(owner.name);
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(bootstrapTokenFile!, 'utf8').trim());
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
      await page.getByRole('button', { name: 'Sign in', exact: true }).click();
      const response = await completed;
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
  mkdirSync(info.project.outputDir, { recursive: true });
  writeFileSync(
    stateFile,
    JSON.stringify(await page.context().storageState()),
    { mode: 0o600 }
  );
}
