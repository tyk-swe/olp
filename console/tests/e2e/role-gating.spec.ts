import {
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test,
  type Page
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import { certifiedModelRecord, ids, now } from './gateway-access-fixtures';

// Fixed roles are enforced by the API, but the console must not offer controls
// the signed-in role cannot use. These specs prove the write affordances are
// gone — not merely rejected after a click.

const apiKey = {
  id: ids.key,
  name: 'production SDK',
  lookup_id: 'olp_live_abcd',
  scopes: ['inference'],
  allowed_routes: [],
  requests_per_minute: null,
  tokens_per_minute: null,
  max_concurrency: null,
  expires_at: null,
  revoked_at: null,
  created_by: ids.user,
  created_by_email: 'owner@example.com',
  created_at: now,
  updated_at: now,
  etag: '01980000-0000-7000-8000-000000000309'
};

const providerSummary = {
  id: ids.provider,
  name: 'production-openai',
  kind: 'openai',
  state: 'active',
  active_revision: 1,
  pending_activation: false,
  enabled_model_count: 1,
  last_probe_at: now,
  last_probe_status: 'succeeded'
};

const activeRoute = {
  id: ids.route,
  slug: 'default',
  created_at: now,
  revision_count: 1,
  latest_revision: {
    id: ids.revision,
    route_id: ids.route,
    revision: 1,
    slug: 'default',
    overall_timeout_ms: 120000,
    max_attempts: 1,
    source_draft_id: ids.draft,
    activated_by: ids.user,
    activated_at: now,
    operations: ['generation'],
    targets: []
  }
};

const routeDraft = {
  id: ids.draft,
  slug: 'staging',
  state: 'draft',
  overall_timeout_ms: 120000,
  max_attempts: 1,
  etag: '01980000-0000-7000-8000-000000000209',
  based_on_revision_id: null,
  operations: ['generation'],
  targets: [],
  created_at: now,
  updated_at: now
};

const members = [
  {
    id: ids.user,
    email: 'developer@example.com',
    display_name: 'Ada Owner',
    role: 'developer',
    active: true,
    etag: '01980000-0000-7000-8000-000000000411',
    created_at: now,
    updated_at: now
  },
  {
    id: ids.developer,
    email: 'grace@example.com',
    display_name: 'Grace Developer',
    role: 'developer',
    active: true,
    etag: '01980000-0000-7000-8000-000000000412',
    created_at: now,
    updated_at: now
  }
];

async function mockReadOnlyApi(page: Page) {
  await page.route('**/api/v1/api-keys**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({ json: { items: [apiKey], next_cursor: null } });
  });
  await page.route('**/api/v1/providers**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({
      json: { items: [providerSummary], next_cursor: null }
    });
  });
  await page.route('**/api/v1/provider-models**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({
      json: {
        items: [
          {
            provider_id: ids.provider,
            provider_name: 'production-openai',
            provider_kind: 'openai',
            model: certifiedModelRecord
          }
        ],
        next_cursor: null
      }
    });
  });
  await page.route('**/api/v1/routes**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({ json: { items: [activeRoute], next_cursor: null } });
  });
  await page.route('**/api/v1/route-drafts**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({ json: { items: [routeDraft], next_cursor: null } });
  });
  await page.route('**/api/v1/users**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({ json: { data: members, next_cursor: null } });
  });
  await page.route('**/api/v1/invitations**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({
      json: {
        data: [
          {
            id: ids.invitation,
            email: 'new@example.com',
            role: 'developer',
            invited_by: ids.user,
            status: 'pending',
            expires_at: '2026-07-13T12:00:00Z',
            created_at: now,
            accepted_at: null
          }
        ],
        next_cursor: null
      }
    });
  });
  await page.route('**/api/v1/sessions?**', async (route) => {
    if (route.request().method() !== 'GET') failUnexpectedApiRequest(route);
    await route.fulfill({
      json: {
        data: [
          {
            id: ids.session,
            user_id: ids.user,
            current: true,
            created_at: now,
            last_seen_at: now,
            expires_at: '2026-07-13T12:00:00Z'
          }
        ],
        next_cursor: null
      }
    });
  });
}

test('a viewer sees API keys without any create, rotate, or revoke control', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'viewer' });
  await mockReadOnlyApi(page);

  await page.goto('/api-keys');

  await expect(
    page.getByRole('cell', { name: 'production SDK' })
  ).toBeVisible();
  await expect(
    page.getByText('Your role can view API keys but not create')
  ).toBeVisible();
  await expect(page.getByRole('link', { name: /Create key/ })).toHaveCount(0);
  for (const action of ['Edit', 'Rotate', 'Revoke']) {
    await expect(page.getByRole('button', { name: action })).toHaveCount(0);
  }
});

test('a developer keeps the API key create control that its role grants', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'developer' });
  await mockReadOnlyApi(page);

  await page.goto('/api-keys');

  await expect(page.getByRole('link', { name: /Create key/ })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Rotate' })).toBeVisible();
  await expect(
    page.getByText('Your role can view API keys but not create')
  ).toHaveCount(0);
});

test('a developer cannot reach provider, route, or model write controls', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'developer' });
  await mockReadOnlyApi(page);

  await page.goto('/providers');
  await expect(
    page.getByText('Your role can view providers but not connect')
  ).toBeVisible();
  await expect(page.getByRole('link', { name: /Add provider/ })).toHaveCount(0);

  await page.goto('/routes');
  await expect(
    page.getByText('Your role can view routes but not create')
  ).toBeVisible();
  await expect(page.getByRole('link', { name: /New route draft/ })).toHaveCount(
    0
  );

  await page.goto('/models');
  await expect(
    page.getByText('Your role can view the model inventory but not change')
  ).toBeVisible();
  await expect(page.getByRole('link', { name: 'Discover models' })).toHaveCount(
    0
  );
  await expect(page.getByRole('checkbox', { name: 'Enabled' })).toBeDisabled();
});

test('an operator keeps the provider and route write controls its role grants', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'operator' });
  await mockReadOnlyApi(page);

  await page.goto('/providers');
  await expect(page.getByRole('link', { name: /Add provider/ })).toBeVisible();

  await page.goto('/routes');
  await expect(
    page.getByRole('link', { name: /New route draft/ })
  ).toBeVisible();

  await page.goto('/models');
  await expect(page.getByRole('checkbox', { name: 'Enabled' })).toBeEnabled();
});

test('a developer reaching the create URLs directly gets no write controls', async ({
  page
}) => {
  await mockProviderKinds(page);
  await mockSession(page, { userId: ids.user, role: 'developer' });
  await mockReadOnlyApi(page);

  // Nav hides these entries, but the URLs stay reachable by hand.
  await page.goto('/providers/new');
  await expect(
    page.getByText('Providers are managed by owners and operators')
  ).toBeVisible();
  await expect(page.getByLabel('Provider name')).toHaveCount(0);

  await page.goto('/routes/new');
  await expect(
    page.getByText('Your role can view this route draft but not change')
  ).toBeVisible();
  await expect(page.getByLabel('Public model slug')).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Create draft' })
  ).toBeDisabled();

  await page.goto('/api-keys/new');
  await expect(
    page.getByText('Your role can view API key policies but not create')
  ).toHaveCount(0);
  await expect(page.getByLabel('Key name')).toBeEnabled();
});

test('a viewer reaching the API key create URL directly cannot submit', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'viewer' });
  await mockReadOnlyApi(page);

  await page.goto('/api-keys/new');

  await expect(
    page.getByText('Your role can view API key policies but not create')
  ).toBeVisible();
  await expect(page.getByLabel('Key name')).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Create and show key' })
  ).toBeDisabled();
});

test('a developer sees access read-only, without invitation or session management', async ({
  page
}) => {
  await mockSession(page, { userId: ids.user, role: 'developer' });
  await mockReadOnlyApi(page);

  await page.goto('/access');

  await expect(page.getByRole('button', { name: 'Invite member' })).toHaveCount(
    0
  );
  await expect(
    page.getByText('Your role can view members but not change roles')
  ).toBeVisible();
  await expect(page.getByLabel('Role for Grace Developer')).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Deactivate' })).toHaveCount(0);

  await page.getByRole('button', { name: 'Invitations' }).click();
  await expect(
    page.getByText('Your role can view invitations but not create')
  ).toBeVisible();
  await expect(page.getByLabel('Email address')).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Create invitation' })
  ).toBeDisabled();

  await page.getByRole('button', { name: 'Sessions' }).click();
  await expect(page.getByLabel('Member')).toHaveCount(0);
});
