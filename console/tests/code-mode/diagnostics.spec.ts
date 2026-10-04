import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import {
  account,
  attempt,
  binding,
  projectId
} from '../../src/lib/features/code-mode/test/fixtures';

test('renders fixture metadata, unknown usage, cursor filters and whole-tree retirement accessibly', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const headers = { 'Cache-Control': 'no-store' };
  await page.route('**/api/v1/project-memberships', (route) =>
    route.fulfill({
      headers,
      json: {
        items: [{ id: projectId, name: 'Diagnostic fixture', role: 'manager' }]
      }
    })
  );
  await page.route('**/api/v1/code/accounts?**', (route) =>
    route.fulfill({ headers, json: { items: [account], next_cursor: null } })
  );
  let retired = false;
  await page.route('**/api/v1/code/bindings?**', (route) =>
    route.fulfill({
      headers,
      json: {
        items: [
          { ...binding, retired_at: retired ? '2026-10-02T00:00:00Z' : null }
        ],
        next_cursor: null
      }
    })
  );
  await page.route(
    `**/api/v1/code/bindings/${binding.id}/retire`,
    async (route) => {
      expect(route.request().method()).toBe('POST');
      expect(route.request().headers()['x-csrf-token']).toBeTruthy();
      expect(route.request().headers()['idempotency-key']).toBeTruthy();
      retired = true;
      await route.fulfill({ status: 204, headers });
    }
  );
  const cursors: Array<string | null> = [];
  await page.route('**/api/v1/code/attempts?**', (route) => {
    const url = new URL(route.request().url());
    expect(url.searchParams.get('project_id')).toBe(projectId);
    cursors.push(url.searchParams.get('cursor'));
    return route.fulfill({
      headers,
      json: {
        items: [attempt],
        next_cursor: url.searchParams.has('cursor') ? null : 'fixture-next'
      }
    });
  });
  await page.goto('/code-mode');
  await expect(
    page.getByText('Unknown — no provider observation')
  ).toBeVisible();
  await page.getByRole('button', { name: 'Attempts', exact: true }).click();
  await expect(
    page.getByText('Retained uncertain reservation', { exact: true })
  ).toBeVisible();
  await expect(
    page
      .locator('dt')
      .filter({ hasText: /^Measured tokens$/ })
      .locator('+ dd')
  ).toHaveText('Unknown');
  await page.getByRole('button', { name: 'Next', exact: true }).click();
  await expect(page.getByText('Page 2', { exact: true })).toBeVisible();
  expect(cursors).toContain('fixture-next');
  await page.getByLabel('Binding ID', { exact: true }).fill(binding.id);
  const filtered = page.waitForRequest(
    (request) =>
      new URL(request.url()).searchParams.get('binding_id') === binding.id
  );
  await page.getByRole('button', { name: 'Apply filters' }).click();
  expect(new URL((await filtered).url()).searchParams.has('cursor')).toBe(
    false
  );
  await page
    .getByRole('button', { name: 'Conversation trees', exact: true })
    .click();
  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', { name: 'Retire entire tree' }).click();
  await expect(
    page.getByRole('heading', { name: 'Retired tree binding' })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Retire entire tree' })
  ).toHaveCount(0);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('code-tree-retired.png'),
    fullPage: true
  });
});
