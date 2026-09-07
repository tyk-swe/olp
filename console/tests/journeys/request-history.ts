import { expect, type Page, type Locator } from '../playwright';

export async function refreshUntilRequestCount(
  page: Page,
  count: number
): Promise<void> {
  const rows = page.locator('table tbody tr');
  await expect
    .poll(
      async () => {
        const current = await rows.count();
        if (current === count) return current;

        const response = page.waitForResponse(
          (candidate) =>
            candidate.request().method() === 'GET' &&
            new URL(candidate.url()).pathname === '/api/v3/requests'
        );
        await page.getByRole('button', { name: 'Refresh' }).click();
        await response;
        return rows.count();
      },
      {
        message: `${count} gateway requests should persist exactly once`,
        timeout: 30_000,
        intervals: [250, 500, 1_000]
      }
    )
    .toBe(count);
}

export async function expectFact(
  scope: Locator,
  label: string,
  value: string
): Promise<void> {
  const term = scope
    .locator('dt')
    .filter({ hasText: new RegExp(`^${label}$`) });
  await expect(term).toHaveCount(1);
  await expect(term.locator('xpath=following-sibling::dd[1]')).toHaveText(
    value
  );
}
