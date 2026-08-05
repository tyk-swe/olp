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

test('model inventory pages the global catalog and updates through provider ETags', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const secondProviderId = '01980000-0000-7000-8000-000000000106';
  const secondModel = {
    ...certifiedModelRecord,
    id: '01980000-0000-7000-8000-000000000107',
    upstream_model: 'claude-sonnet-test',
    display_name: 'Claude Sonnet Test'
  };
  let firstEnabled = true;
  let mutationEtag = '';
  const seenCursors: Array<string | null> = [];

  await page.route('**/api/v1/provider-models**', async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    seenCursors.push(cursor);
    await route.fulfill({
      json:
        cursor === 'opaque-next-model'
          ? {
              items: [
                {
                  provider_id: secondProviderId,
                  provider_name: 'production-anthropic',
                  provider_kind: 'anthropic',
                  model: secondModel
                }
              ],
              next_cursor: null
            }
          : {
              items: [
                {
                  provider_id: ids.provider,
                  provider_name: 'production-openai',
                  provider_kind: 'openai',
                  model: { ...certifiedModelRecord, enabled: firstEnabled }
                }
              ],
              next_cursor: 'opaque-next-model'
            }
    });
  });
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'GET'
    ) {
      await route.fulfill({
        json: providerRecord('active', [
          { ...certifiedModelRecord, enabled: firstEnabled }
        ])
      });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}/models/${ids.model}` &&
      request.method() === 'PATCH'
    ) {
      mutationEtag = (await request.allHeaders())['if-match'];
      firstEnabled = (request.postDataJSON() as { enabled: boolean }).enabled;
      await route.fulfill({
        json: providerRecord('draft', [
          { ...certifiedModelRecord, enabled: firstEnabled }
        ])
      });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto('/models');
  await expect(
    page.getByText('gpt-5.4', { exact: true }).first()
  ).toBeVisible();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('Claude Sonnet Test')).toBeVisible();
  await page.getByRole('button', { name: 'Previous' }).click();
  await page.getByRole('checkbox', { name: 'Enabled' }).uncheck();
  await expect(
    page.getByRole('checkbox', { name: 'Disabled' })
  ).not.toBeChecked();
  expect(seenCursors).toContain('opaque-next-model');
  expect(mutationEtag).toBe('"01980000-0000-7000-8000-000000000109"');
});
