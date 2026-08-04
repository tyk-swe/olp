import {
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  modelRecord,
  providerRecord,
  sessionOptions
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('provider inventory preserves its cursor through detail and wizard navigation', async ({
  page
}) => {
  test.slow();
  await mockSession(page, sessionOptions);
  const firstProvider = providerRecord('active', [modelRecord]);
  const secondProvider = {
    ...firstProvider,
    id: '01980000-0000-7000-8000-000000000105',
    name: 'production-anthropic',
    kind: 'anthropic'
  };
  const seenCursors: Array<string | null> = [];
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === '/api/v1/providers') {
      const cursor = url.searchParams.get('cursor');
      seenCursors.push(cursor);
      await route.fulfill({
        json:
          cursor === 'opaque-next-provider'
            ? { items: [secondProvider], next_cursor: null }
            : { items: [firstProvider], next_cursor: 'opaque-next-provider' }
      });
      return;
    }
    const detailProvider = pathname.includes(secondProvider.id)
      ? secondProvider
      : firstProvider;
    if (pathname === `/api/v1/providers/${detailProvider.id}`) {
      await route.fulfill({ json: detailProvider });
      return;
    }
    if (pathname.endsWith('/models')) {
      await route.fulfill({
        json: { items: detailProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials') || pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto('/providers');
  await expect(
    page.getByText('production-openai', { exact: true })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(
    page.getByText('production-anthropic', { exact: true })
  ).toBeVisible();
  await page.getByRole('link', { name: 'Manage' }).click();
  await expect(
    page.getByRole('heading', { name: 'Connector context' })
  ).toBeVisible();
  await page.getByRole('link', { name: 'All providers' }).click();
  await expect(
    page.getByText('production-anthropic', { exact: true })
  ).toBeVisible();
  await page.getByRole('link', { name: 'Add provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('link', { name: 'Cancel' }).click();
  await expect(
    page.getByText('production-anthropic', { exact: true })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(
    page.getByText('production-openai', { exact: true })
  ).toBeVisible();
  expect(seenCursors).toContain('opaque-next-provider');
});
