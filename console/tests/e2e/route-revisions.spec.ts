import { expect, mockSession, test } from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  ids,
  modelRecord,
  now,
  providerRecord,
  sessionOptions
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('route revision diff and restore-as-draft remain explicit', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const revision = (id: string, number: number, slug: string) => ({
    id,
    route_id: ids.route,
    revision: number,
    slug,
    overall_timeout_ms: number === 1 ? 120000 : 90000,
    max_attempts: 1,
    source_draft_id: ids.draft,
    activated_by: ids.user,
    activated_at: now,
    operations: ['generation'],
    targets: [
      {
        id: ids.target,
        provider_model_id: ids.model,
        provider_id: ids.provider,
        provider_name: 'production-openai',
        provider_model: 'gpt-5.4',
        priority: 1,
        weight: 100,
        timeout_ms: 60000,
        position: 0
      }
    ]
  });
  const revisionTwoId = '01980000-0000-7000-8000-000000000206';
  const history = [
    revision(revisionTwoId, 2, 'default'),
    revision(ids.revision, 1, 'legacy')
  ];
  let restoreCalled = false;

  await page.route('**/api/v1/routes/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/revisions/diff')) {
      await route.fulfill({
        json: {
          from_revision: 1,
          to_revision: 2,
          slug_changed: true,
          timeout_changed: true,
          max_attempts_changed: false,
          operations_added: [],
          operations_removed: [],
          targets_added: [],
          targets_removed: [],
          targets_changed: []
        }
      });
      return;
    }
    if (pathname.endsWith('/restore-as-draft')) {
      restoreCalled = true;
      await route.fulfill({
        status: 201,
        json: {
          id: ids.draft,
          slug: 'default',
          state: 'draft',
          overall_timeout_ms: 90000,
          max_attempts: 1,
          etag: '01980000-0000-7000-8000-000000000209',
          based_on_revision_id: revisionTwoId,
          operations: ['generation'],
          targets: [],
          created_at: now,
          updated_at: now
        }
      });
      return;
    }
    await route.fulfill({ json: { items: history } });
  });
  await page.route('**/api/v1/route-drafts/**', async (route) => {
    await route.fulfill({
      json: {
        id: ids.draft,
        slug: 'default',
        state: 'draft',
        overall_timeout_ms: 90000,
        max_attempts: 1,
        etag: '01980000-0000-7000-8000-000000000209',
        based_on_revision_id: revisionTwoId,
        operations: ['generation'],
        targets: [],
        created_at: now,
        updated_at: now
      }
    });
  });
  await page.route('**/api/v1/providers**', async (route) => {
    await route.fulfill({
      json: {
        items: [providerRecord('active', [modelRecord])],
        next_cursor: null
      }
    });
  });

  page.on('dialog', (dialog) => dialog.accept());
  await page.goto(`/routes/${ids.route}/revisions`);
  await expect(
    page.getByRole('heading', { name: 'Immutable revisions' })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Compare' }).click();
  await expect(page.getByText('slug, deadline')).toBeVisible();
  const newestRow = page.getByRole('row', { name: /Revision 2/ });
  await newestRow.getByRole('button', { name: 'Restore as draft' }).click();
  await expect(page).toHaveURL(new RegExp(`/routes/${ids.draft}$`));
  expect(restoreCalled).toBe(true);
});
