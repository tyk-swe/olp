import AxeBuilder from '@axe-core/playwright';
import { denyClipboard, expect, mockSession, test } from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import { ids, now, sessionOptions } from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('access roles, one-time invitations, sessions, and OIDC are API-backed', async ({
  page
}) => {
  await denyClipboard(page);
  await mockSession(page, sessionOptions);
  const members = [
    {
      id: ids.user,
      email: 'owner@example.com',
      display_name: 'Ada Owner',
      role: 'owner',
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
  let inviteCreated = false;
  let oidcSaved = false;
  let oidcBody: Record<string, unknown> | undefined;

  await page.route('**/api/v1/users**', async (route) => {
    const request = route.request();
    if (request.method() === 'PATCH') {
      const body = request.postDataJSON() as {
        role?: string;
        active?: boolean;
      };
      const updated = {
        ...members[1],
        role: body.role ?? members[1].role,
        active: body.active ?? members[1].active,
        etag: '01980000-0000-7000-8000-000000000413'
      };
      members[1] = updated;
      await route.fulfill({ json: updated });
      return;
    }
    await route.fulfill({ json: { data: members, next_cursor: null } });
  });
  await page.route('**/api/v1/invitations**', async (route) => {
    const request = route.request();
    if (request.method() === 'POST') {
      inviteCreated = true;
      await route.fulfill({
        status: 201,
        json: {
          invitation: {
            id: ids.invitation,
            email: 'new@example.com',
            role: 'developer',
            invited_by: ids.user,
            status: 'pending',
            expires_at: '2026-07-19T12:00:00Z',
            created_at: now,
            accepted_at: null,
            revoked_at: null
          },
          token: 'invite-token-shown-once'
        }
      });
      return;
    }
    await route.fulfill({
      json: {
        data: inviteCreated
          ? [
              {
                id: ids.invitation,
                email: 'new@example.com',
                role: 'developer',
                invited_by: ids.user,
                status: 'pending',
                expires_at: '2026-07-19T12:00:00Z',
                created_at: now,
                accepted_at: null,
                revoked_at: null
              }
            ]
          : [],
        next_cursor: null
      }
    });
  });
  await page.route('**/api/v1/sessions?**', async (route) => {
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
  await page.route('**/api/v1/oidc/configuration', async (route) => {
    const request = route.request();
    if (request.method() === 'GET' && !oidcSaved) {
      await route.fulfill({
        status: 404,
        contentType: 'application/problem+json',
        body: JSON.stringify({ title: 'Not configured', status: 404 })
      });
      return;
    }
    if (request.method() === 'PUT') {
      oidcBody = request.postDataJSON();
      oidcSaved = true;
    }
    await route.fulfill({
      status: oidcSaved ? 200 : 404,
      json: {
        id: ids.oidc,
        discovery_url:
          'https://id.example.com/.well-known/openid-configuration',
        issuer: 'https://id.example.com',
        client_id: 'olp-console',
        has_client_secret: true,
        enabled: true,
        scopes: ['openid', 'profile', 'email'],
        email_claim: 'email',
        groups_claim: 'groups',
        default_role: 'viewer',
        email_role_mappings: [],
        group_role_mappings: [{ claim_value: 'platform', role: 'operator' }],
        etag: '01980000-0000-7000-8000-000000000414'
      }
    });
  });
  await page.route('**/api/v1/oidc/link', async (route) => {
    await route.fulfill({ json: { authorization_url: '/oidc-test-redirect' } });
  });
  await page.route('**/api/v1/oidc/identities', async (route) => {
    await route.fulfill({
      json: { data: [], linking_available: true, has_local_password: true }
    });
  });
  await page.route('**/api/v1/profile/reauthenticate', async (route) => {
    expect(route.request().postDataJSON()).toEqual({
      current_password: 'correct horse battery staple',
      purpose: 'oidc_link'
    });
    await route.fulfill({ status: 204 });
  });

  await page.goto('/access');
  await expect(
    page.getByRole('heading', { name: 'Access', exact: true })
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByLabel('Role for Grace Developer').selectOption('operator');
  await expect(
    page.getByText('Grace Developer is now operator.')
  ).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  const deactivateGrace = page
    .getByRole('row', { name: /Grace Developer/ })
    .getByRole('button', { name: 'Deactivate' });
  await deactivateGrace.focus();
  await deactivateGrace.press('Enter');
  await expect(
    page.getByText(
      'Grace Developer was deactivated and existing sessions were revoked.'
    )
  ).toBeVisible();

  await page.getByRole('button', { name: 'Invitations' }).click();
  await page.getByPlaceholder('person@example.com').fill('new@example.com');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitationDialog = page.getByRole('dialog', {
    name: 'Copy the invitation link now.'
  });
  await expect(
    invitationDialog.getByText('invite-token-shown-once')
  ).toBeVisible();
  await invitationDialog
    .getByRole('button', { name: 'Copy invitation link' })
    .click();
  await expect(invitationDialog.getByRole('alert')).toContainText(
    'Clipboard access is unavailable. Copy this invitation link manually.'
  );
  await expect(
    invitationDialog.getByText(
      /\/invitations\/accept#token=invite-token-shown-once$/
    )
  ).toBeVisible();
  await invitationDialog
    .getByRole('button', { name: 'I have shared it' })
    .click();
  await expect(page.getByText('invite-token-shown-once')).toHaveCount(0);

  await page.getByRole('button', { name: 'Sessions' }).click();
  await expect(page.getByText(ids.session)).toBeVisible();
  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Expected issuer').fill('https://id.example.com');
  await page
    .getByLabel('Discovery URL')
    .fill('https://id.example.com/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('olp-console');
  await page.getByLabel('Client secret').fill('oidc-write-only-secret');
  await page.getByLabel('Enabled').check();
  await page.getByLabel('Group mappings').fill('platform=operator');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration validated and enabled.')
  ).toBeVisible();
  expect(oidcBody).toMatchObject({
    client_secret: 'oidc-write-only-secret',
    enabled: true,
    group_role_mappings: [{ claim_value: 'platform', role: 'operator' }]
  });
  await expect(page.getByLabel('Client secret')).toHaveValue('');
  await page.getByRole('button', { name: 'Link my identity' }).click();
  const reauthentication = page.getByRole('dialog', {
    name: 'Confirm your identity'
  });
  await reauthentication
    .getByLabel('Current password')
    .fill('correct horse battery staple');
  await reauthentication.getByRole('button', { name: 'Confirm' }).click();
  await expect(page).toHaveURL(/\/oidc-test-redirect$/);
});

test('new OIDC configuration leaves the sentinel state and versions later saves', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let current: Record<string, unknown> | null = null;
  const saveEtags: Array<string | undefined> = [];

  await page.route('**/api/v1/users**', async (route) => {
    await route.fulfill({ json: { data: [], next_cursor: null } });
  });
  await page.route('**/api/v1/oidc/configuration', async (route) => {
    const request = route.request();
    if (request.method() === 'GET' && !current) {
      await route.fulfill({
        status: 404,
        contentType: 'application/problem+json',
        body: JSON.stringify({ title: 'Not configured', status: 404 })
      });
      return;
    }
    if (request.method() === 'PUT') {
      saveEtags.push((await request.allHeaders())['if-match']);
      const input = request.postDataJSON() as Record<string, unknown>;
      current = {
        id: ids.oidc,
        ...input,
        has_client_secret: Boolean(input.client_secret),
        etag:
          saveEtags.length === 1
            ? '01980000-0000-7000-8000-000000000451'
            : '01980000-0000-7000-8000-000000000452'
      };
    }
    await route.fulfill({ json: current });
  });

  await page.goto('/access');
  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Expected issuer').fill('https://id.example.test');
  await page
    .getByLabel('Discovery URL')
    .fill('https://id.example.test/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('olp-console');
  await page.getByLabel('Client secret').fill('write-only-secret');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration saved but disabled.')
  ).toBeVisible();
  await expect(page.getByLabel('Client secret')).toHaveValue('');

  await page.getByLabel('Client ID').fill('olp-console-v2');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  expect(saveEtags).toEqual([
    undefined,
    '"01980000-0000-7000-8000-000000000451"'
  ]);
});

test('failed OIDC conflict reload preserves edits and write-only secret', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let failNextReload = false;
  let current = {
    id: ids.oidc,
    discovery_url: 'https://id.example.test/.well-known/openid-configuration',
    issuer: 'https://id.example.test',
    client_id: 'remote-client',
    has_client_secret: true,
    enabled: true,
    scopes: ['openid', 'profile', 'email'],
    email_claim: 'email',
    groups_claim: 'groups',
    default_role: 'viewer',
    email_role_mappings: [],
    group_role_mappings: [],
    etag: '01980000-0000-7000-8000-000000000461'
  };

  await page.route('**/api/v1/users**', async (route) => {
    await route.fulfill({ json: { data: [], next_cursor: null } });
  });
  await page.route('**/api/v1/oidc/configuration', async (route) => {
    const request = route.request();
    if (request.method() === 'GET' && failNextReload) {
      failNextReload = false;
      await route.fulfill({
        status: 503,
        json: { title: 'OIDC reload unavailable', status: 503 }
      });
      return;
    }
    if (request.method() === 'PUT') {
      current = {
        ...current,
        client_id: 'remote-client-v2',
        etag: '01980000-0000-7000-8000-000000000462'
      };
      failNextReload = true;
      await route.fulfill({
        status: 412,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          type: 'https://openllmproxy.dev/problems/etag_mismatch',
          title: 'The OIDC configuration changed elsewhere',
          status: 412
        })
      });
      return;
    }
    await route.fulfill({ json: current });
  });

  await page.goto('/access');
  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Client ID').fill('local-client');
  await page.getByLabel('Client secret').fill('local-write-only-secret');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(page.getByRole('alert')).toContainText(
    'This item changed elsewhere.'
  );

  await page.getByRole('button', { name: 'Reload' }).click();
  await expect(page.getByText('OIDC reload unavailable')).toBeVisible();
  await page.getByRole('button', { name: 'Retry' }).click();
  await expect(page.getByLabel('Client ID')).toHaveValue('local-client');
  await expect(page.getByLabel('Client secret')).toHaveValue(
    'local-write-only-secret'
  );
  await page.getByRole('button', { name: 'Reload' }).click();
  await expect(page.getByLabel('Client ID')).toHaveValue('remote-client-v2');
  await expect(page.getByLabel('Client secret')).toHaveValue('');
});
