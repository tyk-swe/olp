import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';
import { emulateTwoHundredPercentZoom } from '../playwright';

const iframe = (id: string) => `/iframe.html?id=${id}&viewMode=story`;

test('operator primitives are interactive, keyboard complete, and axe clean', async ({ page }) => {
  await page.goto(iframe('foundation-operator-primitives--interactive'));
  await expect(page.getByRole('heading', { name: 'Operator primitives' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByRole('radio', { name: 'Text' }).focus();
  await page.keyboard.press('ArrowRight');
  await expect(page.getByText('Selected mode:')).toContainText('tools');

  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('Page 2')).toBeVisible();

  await page.getByRole('button', { name: 'Use dark theme' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');

  await page.getByRole('button', { name: 'Reveal generated key' }).click();
  await expect(page.getByRole('dialog')).toContainText('Copy this key now');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByRole('button', { name: 'I saved the key' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

for (const state of ['loading', 'empty', 'error'] as const) {
  test(`${state} state is axe clean in forced colors and at 200% zoom`, async ({ page }) => {
    await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
    await page.goto(iframe(`foundation-async-states--${state}`));
    await emulateTwoHundredPercentZoom(page);
    await expect(page.getByRole('heading', { name: 'Operational state' })).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  });
}
