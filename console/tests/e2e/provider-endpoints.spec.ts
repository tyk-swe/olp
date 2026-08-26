import {
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  certifiedModelRecord,
  ids,
  providerRecord,
  sessionOptions
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('native provider detail never round-trips its official endpoint as a custom endpoint', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let updateBody: Record<string, unknown> | undefined;
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    endpoint: 'https://api.openai.com/v1/'
  });
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/credentials')) {
      await route.fulfill({ json: { items: [] } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'PATCH'
    ) {
      updateBody = request.postDataJSON();
      currentProvider = { ...currentProvider, state: 'draft', endpoint: null };
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(
    page.getByRole('heading', { name: 'Connector context' })
  ).toBeVisible();
  await expect(page.getByLabel('Endpoint', { exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByText('Provider draft settings saved.')).toBeVisible();
  expect(updateBody).toMatchObject({
    name: 'production-openai',
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    auth_mode: 'api_key'
  });
});

test('provider validation rejections name the field, the reason, and the code', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const currentProvider = providerRecord('active', [certifiedModelRecord], {
    endpoint: 'https://api.openai.com/v1/'
  });
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/credentials')) {
      await route.fulfill({ json: { items: [] } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'PATCH'
    ) {
      await route.fulfill({
        status: 422,
        contentType: 'application/problem+json',
        json: {
          type: 'https://openllmproxy.dev/problems/validation_failed',
          title: 'Validation failed',
          status: 422,
          detail: 'One or more fields are invalid.',
          errors: {
            cloud_region: ['This connector does not accept a region.'],
            endpoint: ['The endpoint must use https.']
          },
          // `error_codes` always pairs with `errors`; an empty string is the
          // API's way of saying this message carries no code.
          error_codes: { cloud_region: ['forbidden'], endpoint: [''] }
        }
      });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(
    page.getByRole('heading', { name: 'Connector context' })
  ).toBeVisible();
  await expect(page.getByText('Created by owner@example.com')).toBeVisible();

  await page.getByRole('button', { name: 'Save draft' }).click();
  const problem = page
    .getByRole('alert')
    .filter({ hasText: 'One or more fields are invalid.' });
  await expect(problem).toContainText('One or more fields are invalid.');
  await expect(problem).toContainText(
    'This connector does not accept a region.'
  );
  await expect(problem.getByText('forbidden', { exact: true })).toBeVisible();
  await expect(problem).toContainText('The endpoint must use https.');
  // The uncoded message renders without an empty code chip beside it.
  await expect(problem.locator('code')).toHaveCount(1);
});
