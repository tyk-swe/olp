import AxeBuilder from '@axe-core/playwright';
import type { ApiKey } from '../../src/lib/api/management/api-keys';
import { expect, mockSession, test, type Page } from '../playwright';
import { ids, now, sessionOptions } from './gateway-access-fixtures';

function key(id: string, issuer: string, name: string): ApiKey {
  return {
    id,
    lookup_id: `issuer_${id.slice(-6)}`,
    name,
    created_by: issuer,
    created_by_email:
      issuer === ids.user ? 'owner@example.com' : 'inactive@example.com',
    scopes: ['inference'],
    allowed_routes: [],
    requests_per_minute: null,
    tokens_per_minute: null,
    max_concurrency: null,
    budget: {
      daily: { limit: '5', accrued: '1', window_ends_at: now },
      monthly: { limit: null, accrued: '1', window_ends_at: now },
      unpriced_attempts: 0
    },
    expires_at: null,
    revoked_at: null,
    rotated_at: null,
    etag: id,
    created_at: now
  };
}

async function mockIssuerPages(page: Page) {
  const state = { directoryRequests: 0, requests: [] as URLSearchParams[] };
  const ownerKey = key(ids.key, ids.user, 'Owner key');
  const inactiveKey = key(
    ids.model,
    ids.developer,
    'Inactive issuer first key'
  );
  const nextKey = key(
    ids.provider,
    ids.developer,
    'Inactive issuer second key'
  );
  await page.route('**/api/v1/users**', async (route) => {
    state.directoryRequests += 1;
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/api-keys**', async (route) => {
    expect(route.request().method()).toBe('GET');
    const query = new URL(route.request().url()).searchParams;
    state.requests.push(query);
    const issuer = query.get('created_by');
    const cursor = query.get('cursor');
    const items =
      issuer === ids.developer
        ? [cursor ? nextKey : inactiveKey]
        : issuer && issuer !== ids.user
          ? []
          : [cursor ? inactiveKey : ownerKey];
    await route.fulfill({
      json: {
        items,
        next_cursor:
          !items.length || cursor || issuer === ids.user ? null : 'next-page'
      }
    });
  });
  return state;
}

for (const role of ['developer', 'viewer'] as const) {
  test(`${role} filters attributed keys without requesting the user directory`, async ({
    page
  }) => {
    await mockSession(page, { ...sessionOptions, role });
    const state = await mockIssuerPages(page);
    const requests = state.requests;
    await page.goto('/api-keys');
    await expect(page.getByRole('row', { name: /Owner key/ })).toBeVisible();
    await page.getByRole('button', { name: 'Next' }).click();
    await expect(
      page.getByRole('row', { name: /Inactive issuer first key/ })
    ).toBeVisible();
    await expect(
      page.locator(`datalist option[value="${ids.developer}"]`)
    ).toHaveText('inactive@example.com');
    await page.getByLabel('Issuer (user ID)').fill(ids.developer);
    await page.getByRole('button', { name: 'Apply issuer' }).click();
    await expect(page).toHaveURL(new RegExp(`created_by=${ids.developer}$`));
    await expect
      .poll(() => requests.at(-1)?.get('created_by'))
      .toBe(ids.developer);
    expect(requests.at(-1)?.has('cursor')).toBe(false);
    await expect(page.getByRole('button', { name: 'Previous' })).toBeDisabled();
    await page.getByRole('button', { name: 'Next' }).click();
    await expect(
      page.getByRole('row', { name: /Inactive issuer second key/ })
    ).toBeVisible();
    await page.getByLabel('Issuer (user ID)').fill(ids.user);
    await page.getByRole('button', { name: 'Apply issuer' }).click();
    await expect(page.getByRole('row', { name: /Owner key/ })).toBeVisible();
    expect(requests.at(-1)?.get('created_by')).toBe(ids.user);
    expect(requests.at(-1)?.has('cursor')).toBe(false);
    await expect(
      page.getByRole('row', { name: /Inactive issuer/ })
    ).toHaveCount(0);
    await page.getByLabel('Issuer (user ID)').fill(ids.invitation);
    await page.getByRole('button', { name: 'Apply issuer' }).click();
    await expect(
      page.getByRole('heading', { name: 'No API keys for this issuer' })
    ).toBeVisible();
    await page.getByRole('button', { name: 'Clear issuer' }).click();
    await expect(page).toHaveURL(/\/api-keys$/);
    await expect(page.getByRole('row', { name: /Owner key/ })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Previous' })).toBeDisabled();
    await page.goBack();
    await expect(page.getByLabel('Issuer (user ID)')).toHaveValue(
      ids.invitation
    );
    await expect(
      page.getByRole('heading', { name: 'No API keys for this issuer' })
    ).toBeVisible();
    await page.goForward();
    await expect(page.getByRole('row', { name: /Owner key/ })).toBeVisible();
    await expect(
      page.getByRole('button', { name: 'Rotate', exact: true })
    ).toHaveCount(role === 'viewer' ? 0 : 1);
    await expect(
      page.getByRole('button', { name: 'Revoke', exact: true })
    ).toHaveCount(role === 'viewer' ? 0 : 1);
    expect(state.directoryRequests).toBe(0);
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  });
}
