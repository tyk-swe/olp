import AxeBuilder from '@axe-core/playwright';
import { expect, type Page, type TestInfo } from '../playwright';

export async function verifyConsolePolish(page: Page, info: TestInfo) {
  await page.setViewportSize({ width: 320, height: 800 });
  const trigger = page.getByRole('button', { name: 'Open navigation' });
  await trigger.click();
  const navigation = page.getByRole('dialog', {
    name: 'Navigation',
    exact: true
  });
  await expect(navigation).toBeVisible();
  await expect(trigger).toHaveAttribute('aria-expanded', 'true');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.keyboard.press('Escape');
  await expect(navigation).toBeHidden();
  await expect(trigger).toBeFocused();
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  await trigger.click();
  await page.setViewportSize({ width: 1440, height: 900 });
  await expect(navigation).toBeHidden();
  await expect(page.locator('.topbar .brand')).toBeFocused();

  await page.goto('/api-keys/new');
  await page.getByRole('button', { name: /Create and show key/ }).click();
  await expect(page.getByLabel('Key name')).toBeFocused();
  await page.getByLabel('Key name').fill('Unsaved validation check');
  await page.getByLabel('Requests per minute', { exact: true }).fill('0');
  await page.getByRole('button', { name: /Create and show key/ }).click();
  await expect(
    page.getByLabel('Requests per minute', { exact: true })
  ).toBeFocused();
  await expect(page.getByLabel('Key name')).toHaveValue(
    'Unsaved validation check'
  );
  await expect(
    page.getByLabel('Requests per minute', { exact: true })
  ).toHaveAttribute('aria-describedby', 'rpm-error');
  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('link', { name: 'Cancel', exact: true }).click();
  await page
    .getByLabel('Issuer (user ID)')
    .fill('00000000-0000-4000-8000-000000000000');
  await page.getByRole('button', { name: 'Apply issuer' }).click();
  const empty = page.locator('.empty-state');
  await expect(
    empty.getByRole('heading', { name: 'No API keys for this issuer' })
  ).toBeVisible();
  await empty.getByRole('button', { name: 'Clear issuer' }).click();
  await expect(page.getByLabel('Issuer (user ID)')).toHaveValue('');
  await expect(page.locator('.key-table')).toBeVisible();

  const screens = [
    ['/', 'Gateway overview'],
    ['/providers', 'Providers'],
    ['/providers/new', 'Connect an upstream provider.'],
    ['/routes', 'Routes'],
    ['/routes/new', 'Build a route draft.'],
    ['/api-keys', 'API Keys'],
    ['/api-keys/new', 'Create a proxy key.']
  ] as const;
  await page.emulateMedia({ reducedMotion: 'reduce' });
  for (const width of [320, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    for (const [path, heading] of screens) {
      await page.goto(path);
      await expect(
        page.getByRole('heading', { name: heading, exact: true })
      ).toBeVisible();
      await expect(page.locator('.loading-state')).toHaveCount(0);
      if (path === '/providers/new')
        await expect(page.locator('form.editor')).toBeVisible();
      if (path === '/routes/new')
        await expect(page.locator('form.studio')).toBeVisible();
      if (path === '/api-keys/new')
        await expect(page.locator('form.key-form')).toBeVisible();
      if (path === '/') {
        // Stress intrinsic sizing without changing the installation or account.
        const original = await page.evaluate(() => {
          return ['.account-label', '.edition-name', '.endpoint-row code'].map(
            (selector) => {
              const node = document.querySelector(selector)!;
              const text = node.textContent;
              node.textContent = 'LongName'.repeat(20);
              return { selector, text };
            }
          );
        });
        expect
          .soft(
            await page.evaluate(
              () => document.documentElement.scrollWidth - window.innerWidth
            ),
            `Long names and endpoint at ${width}px`
          )
          .toBeLessThanOrEqual(0);
        await page.evaluate((entries) => {
          for (const { selector, text } of entries)
            document.querySelector(selector)!.textContent = text;
        }, original);
      }
      expect
        .soft(
          await page.evaluate(
            () => document.documentElement.scrollWidth - window.innerWidth
          ),
          `${path} at ${width}px must not widen the page`
        )
        .toBeLessThanOrEqual(0);
      if (width === 320 || width === 1440) {
        expect
          .soft(
            (await new AxeBuilder({ page }).analyze()).violations,
            `${path} accessibility at ${width}px`
          )
          .toEqual([]);
        await page.screenshot({
          path: info.outputPath(
            `polish-${path.slice(1).replaceAll('/', '-') || 'overview'}-${width}.png`
          ),
          fullPage: true
        });
      }
    }
  }
}
