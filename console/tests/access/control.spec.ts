import { readFileSync } from 'node:fs';
import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const password = 'a long browser test password';
const rotatedPassword = 'a rotated browser test password';
// Every signed-in role lands on the overview;
// only roles that can manage providers see the onboarding heading.
const ownerLanding = 'Bring your first model route online.';
const viewerLanding = 'Gateway overview';

async function changeRemote(
  page: Page,
  path: string,
  method: 'PATCH' | 'PUT',
  body: Record<string, unknown>
) {
  const status = await page.evaluate(
    async ({ path, method, body }) => {
      const [resource, session] = await Promise.all([
        fetch(path).then((response) => response.json()),
        fetch('/api/v1/sessions/current').then((response) => response.json())
      ]);
      const response = await fetch(path, {
        method,
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': session.csrf_token,
          'If-Match': `"${resource.etag}"`
        },
        body: JSON.stringify(body)
      });
      return response.status;
    },
    { path, method, body }
  );
  expect(status).toBe(200);
}

async function signIn(page: Page, email: string) {
  const deadline = Date.now() + 75_000;
  while (true) {
    await page.getByLabel('Email').fill(email);
    await page.getByLabel('Password', { exact: true }).fill(password);
    const completed = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === '/api/v1/sessions'
    );
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    const response = await completed;
    if (response.status() !== 429 || Date.now() >= deadline) {
      expect(response.status()).toBe(201);
      break;
    }
    const seconds = Number(response.headers()['retry-after'] ?? '1');
    await new Promise((resolve) =>
      setTimeout(resolve, Math.min(60, Math.max(1, seconds)) * 1000)
    );
  }
}

async function signOut(page: Page) {
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page
    .getByRole('banner')
    .getByRole('button', { name: 'Sign out', exact: true })
    .click();
  await expect(page).toHaveURL(/\/login/);
}

test('setup, invitations, key policy, profile, settings, audit, and OIDC work through this origin', async ({
  page,
  browser
}, info) => {
  const failures: string[] = [];
  page.on('pageerror', (error) => failures.push(error.message));
  page.on('response', (response) => {
    if (response.url().includes('/api/v1/') && response.status() >= 500)
      failures.push(`${response.status()} ${new URL(response.url()).pathname}`);
  });
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill('Owner');
  await page.getByLabel('Work email').fill('owner@example.com');
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page
    .getByLabel('Setup token')
    .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
  await page.getByRole('button', { name: 'Create owner account' }).click();
  await expect(page.getByRole('heading', { name: ownerLanding })).toBeVisible();
  await page.screenshot({
    path: info.outputPath('overview.png'),
    fullPage: true
  });

  await page.goto('/access');
  await page.getByRole('button', { name: 'Invite member' }).click();
  await page.getByLabel('Email address').fill('viewer@example.com');
  await page.getByLabel('Role').selectOption('viewer');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitation = page.getByRole('dialog', {
    name: 'Copy the invitation link now.'
  });
  const link = (await invitation
    .locator('.invitation-token')
    .textContent())!.trim();
  await invitation.getByRole('button', { name: 'I have shared it' }).click();
  const invitedContext = await browser.newContext({
    baseURL: info.project.use.baseURL
  });
  const invited = await invitedContext.newPage();
  await invited.goto(link);
  await invited.getByLabel('Display name').fill('Viewer');
  await invited.getByLabel('Password', { exact: true }).fill(password);
  await invited.getByLabel('Confirm password').fill(password);
  await invited.getByRole('button', { name: 'Accept invitation' }).click();
  await expect(
    invited.getByRole('heading', { name: viewerLanding })
  ).toBeVisible();
  expect(
    await invited.evaluate(async () => (await fetch('/api/v1/users')).status)
  ).toBe(403);
  // Invitation success must remain usable after the initial session ends.
  await signOut(invited);
  await signIn(invited, 'viewer@example.com');
  await expect(
    invited.getByRole('heading', { name: viewerLanding })
  ).toBeVisible();
  await invited.goto('/requests');
  await expect(
    invited.getByRole('link', { name: 'Audit', exact: true })
  ).not.toHaveCount(0);
  await invitedContext.close();

  // Bulk operations keep the normal member API's ETags and session revocation.
  await page.getByRole('button', { name: 'Members', exact: true }).click();
  await page
    .getByRole('button', { name: 'Refresh members', exact: true })
    .click();
  await expect(page.getByLabel('Select Owner', { exact: true })).toBeDisabled();
  for (const action of ['role:developer', 'role:viewer']) {
    await page.getByLabel('Select Viewer', { exact: true }).check();
    await page.getByLabel('Bulk member action').selectOption(action);
    await page
      .getByRole('button', { name: 'Apply to selected members' })
      .click();
    await expect(
      page.getByText('1 of 1 members updated.', { exact: false })
    ).toBeVisible();
    await expect(page.getByLabel('Role for Viewer')).toHaveValue(
      action.slice(5)
    );
  }

  await page.goto('/usage');
  await page.getByText('Saved views', { exact: true }).click();
  await page.getByLabel('View name', { exact: true }).fill('Session view');
  await page.getByRole('button', { name: 'Save current filters' }).click();
  await page.reload();
  await page.getByText('Saved views', { exact: true }).click();
  await page
    .getByLabel('Saved view', { exact: true })
    .selectOption('Session view');
  await page.getByRole('button', { name: 'Apply view', exact: true }).click();
  await signOut(page);
  await signIn(page, 'owner@example.com');
  await page.goto('/usage');
  await page.getByText('Saved views', { exact: true }).click();
  await expect(page.getByRole('option', { name: 'Session view' })).toHaveCount(
    0
  );
  await page.goto('/access');

  // The console offers only what the server would admit: with an assigned
  // access scope, installation pages such as Audit and Settings disappear.
  const viewerID = await page.evaluate(async () => {
    const users = await (await fetch('/api/v1/users')).json();
    return users.items.find(
      (user: { email: string }) => user.email === 'viewer@example.com'
    ).id as string;
  });
  await changeRemote(page, `/api/v1/users/${viewerID}`, 'PATCH', {
    access_scope: 'assigned'
  });
  const assignedContext = await browser.newContext({
    baseURL: info.project.use.baseURL
  });
  const assigned = await assignedContext.newPage();
  await assigned.goto('/login');
  await signIn(assigned, 'viewer@example.com');
  await expect(
    assigned.getByRole('heading', { name: viewerLanding })
  ).toBeVisible();
  await assigned.goto('/requests');
  await expect(
    assigned.getByRole('heading', { name: 'Request Explorer' })
  ).toBeVisible();
  for (const page of ['Audit', 'Settings'])
    await expect(
      assigned.getByRole('link', { name: page, exact: true })
    ).toHaveCount(0);
  await assigned.getByRole('button', { name: 'Open account menu' }).click();
  await expect(
    assigned.getByRole('link', { name: 'Installation settings' })
  ).toHaveCount(0);
  await assigned.screenshot({
    path: info.outputPath('assigned-navigation.png'),
    fullPage: true
  });
  // Project policy administration is available without installation-wide access.
  const delegatedProject = await page.evaluate(async (viewerID) => {
    const session = await (await fetch('/api/v1/sessions/current')).json();
    const headers = {
      'Content-Type': 'application/json',
      'X-CSRF-Token': session.csrf_token,
      'Idempotency-Key': crypto.randomUUID()
    };
    const created = await fetch('/api/v1/projects', {
      method: 'POST',
      headers,
      body: JSON.stringify({ name: 'Delegated policy' })
    });
    if (created.status !== 201)
      throw new Error(`create project: ${created.status}`);
    const project = await created.json();
    const member = await fetch(
      `/api/v1/projects/${project.id}/members/${viewerID}`,
      {
        method: 'PUT',
        headers: { ...headers, 'If-Match': `"${project.etag}"` },
        body: JSON.stringify({ role: 'manager' })
      }
    );
    if (member.status !== 200)
      throw new Error(`project membership: ${member.status}`);
    return project.id as string;
  }, viewerID);
  await assigned.getByRole('button', { name: 'Open account menu' }).click();
  await assigned.getByRole('link', { name: 'Access', exact: true }).click();
  await assigned
    .getByRole('link', { name: 'Project policies', exact: true })
    .click();
  await expect(
    assigned.getByRole('heading', { name: 'Project policies' })
  ).toBeVisible();
  await expect(assigned.getByLabel('Enable end-user policy')).toBeDisabled();
  await expect(assigned.getByLabel('Required attribution keys')).toBeDisabled();
  await expect(
    assigned.getByRole('button', { name: 'Save end-user policy' })
  ).toHaveCount(0);
  await changeRemote(page, `/api/v1/users/${viewerID}`, 'PATCH', {
    role: 'developer'
  });
  await assigned.goto('/login');
  await signIn(assigned, 'viewer@example.com');
  await assigned.waitForURL((url) => url.pathname !== '/login');
  await assigned.goto('/project-policies');
  await assigned.getByLabel('Enable end-user policy').check();
  await assigned.getByLabel('Requests per minute', { exact: true }).fill('9');
  await assigned.getByRole('button', { name: 'Save end-user policy' }).click();
  await expect(
    assigned.getByText('End-user policy saved.', { exact: true })
  ).toBeVisible();
  const delegatedPolicy = await assigned.evaluate(
    async (projectId) =>
      (await fetch(`/api/v1/projects/${projectId}/end-user-policy`)).json(),
    delegatedProject
  );
  expect(delegatedPolicy.policy.defaults.requests_per_minute).toBe(9);
  const attributionPanel = assigned.getByRole('region', {
    name: 'Project attribution policy'
  });
  await attributionPanel.getByLabel('Required attribution keys').fill('team');
  await attributionPanel
    .getByRole('button', { name: 'Add pinned label' })
    .click();
  await attributionPanel
    .getByLabel('Pinned label', { exact: true })
    .fill('team');
  await attributionPanel
    .getByLabel('Pinned value', { exact: true })
    .fill('core');
  await attributionPanel
    .getByRole('button', { name: 'Save attribution policy' })
    .click();
  await expect(
    attributionPanel.getByText('Attribution policy saved.', { exact: true })
  ).toBeVisible();
  const savedAttribution = await assigned.evaluate(
    async (id) =>
      (await fetch(`/api/v1/projects/${id}/attribution-policy`)).json(),
    delegatedProject
  );
  expect(savedAttribution.policy.attribution_defaults).toEqual({
    team: 'core'
  });

  const groupsPanel = assigned.getByRole('region', {
    name: 'Project route groups'
  });
  await groupsPanel.getByRole('button', { name: 'Add route group' }).click();
  await groupsPanel
    .getByLabel('Group name', { exact: true })
    .fill('production');
  await groupsPanel
    .getByLabel('Member routes', { exact: true })
    .fill('future-chat, future-embeddings');
  await groupsPanel.getByRole('button', { name: 'Save route groups' }).click();
  await expect(
    groupsPanel.getByText('Route groups saved.', { exact: true })
  ).toBeVisible();
  const savedGroups = await assigned.evaluate(
    async (id) => (await fetch(`/api/v1/projects/${id}/route-groups`)).json(),
    delegatedProject
  );
  expect(savedGroups.groups.production).toEqual([
    'future-chat',
    'future-embeddings'
  ]);
  expect(
    (await new AxeBuilder({ page: assigned }).analyze()).violations
  ).toEqual([]);
  await assigned.evaluate(async () => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
    await new Promise<void>((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
    );
  });
  await assigned.screenshot({
    path: info.outputPath('project-end-user-policy.png'),
    fullPage: true
  });
  await changeRemote(page, `/api/v1/users/${viewerID}`, 'PATCH', {
    role: 'viewer'
  });
  await assigned.goto('/login');
  await signIn(assigned, 'viewer@example.com');
  await assigned.waitForURL((url) => url.pathname !== '/login');
  await assigned.goto('/project-policies');
  await expect(
    assigned.getByLabel('Requests per minute', { exact: true })
  ).toBeDisabled();
  await expect(
    assigned.getByRole('button', { name: 'Add route group', exact: true })
  ).toBeDisabled();
  await expect(
    assigned.getByLabel('Member routes', { exact: true })
  ).toBeDisabled();
  await expect(
    assigned.getByRole('button', { name: 'Save route groups', exact: true })
  ).toHaveCount(0);
  await assignedContext.close();

  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('Browser application');
  await page
    .getByLabel('Project', { exact: true })
    .selectOption(delegatedProject);
  await page
    .getByLabel('Allowed route groups', { exact: true })
    .fill('production');
  await page.getByLabel('Required attribution keys').fill('team');
  await page.getByRole('button', { name: 'Add pinned label' }).click();
  await page.getByLabel('Pinned label', { exact: true }).fill('team');
  await page.getByLabel('Pinned value', { exact: true }).fill('core');

  await expect(page.getByText('No routes are configured yet.')).toBeVisible();
  await page.getByLabel('Requests per minute').fill('60');
  await page.getByLabel('Allowed client networks').fill('127.0.0.0/8\n::1/128');
  await page
    .getByLabel('Identifier source', { exact: true })
    .selectOption('header');
  await page.getByLabel('Rotation reminder interval (days)').fill('30');
  await page.getByLabel('Enable end-user policy', { exact: true }).check();
  await page
    .getByRole('group', { name: 'Per-end-user limits', exact: true })
    .getByLabel('Requests per minute', { exact: true })
    .fill('12');
  await page.evaluate(async () => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
    await new Promise<void>((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
    );
  });
  await page.screenshot({
    path: info.outputPath('key-end-user-policy.png'),
    fullPage: true
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  const createdKey = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/api-keys') &&
      response.request().method() === 'POST'
  );
  await page
    .getByRole('button', { name: 'Create and show key', exact: true })
    .click();
  const secret = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(secret).toBeVisible();
  const keyId = (await (await createdKey).json()).id;
  const savedSource = await page.evaluate(async (id) => {
    const saved = await fetch(`/api/v1/api-keys/${id}`);
    return await saved.json();
  }, keyId);
  expect(savedSource.end_user_source).toBe('header');
  expect(savedSource.rotation_interval_days).toBe(30);
  expect(savedSource.rotation_due_at).toBeTruthy();
  expect(savedSource.required_attribution_keys).toEqual(['team']);
  expect(savedSource.attribution_defaults).toEqual({ team: 'core' });
  expect(savedSource.allowed_route_groups).toEqual(['production']);
  expect(savedSource.allowed_cidrs).toEqual(['127.0.0.0/8', '::1/128']);
  expect(savedSource.end_user_policy.defaults.requests_per_minute).toBe(12);
  await expect(
    secret.getByRole('button', { name: 'Run connection test' })
  ).toBeVisible();
  await secret.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(
    page
      .getByRole('region', { name: 'API keys' })
      .getByText('Browser application', { exact: true })
  ).toBeVisible();
  // Valkey is configured for this installation, so limit policies are live
  // accounting rather than saved intent: the inventory reports this key's
  // budget state instead of the not-enforced note.
  await expect(page.getByText('No cost budget')).toBeVisible();
  await page.screenshot({ path: info.outputPath('keys.png'), fullPage: true });

  const keyRow = page
    .getByRole('row')
    .filter({ hasText: 'Browser application' });
  await expect(
    keyRow.getByText('Groups: production', { exact: true })
  ).toBeVisible();
  await keyRow.getByRole('button', { name: 'Rotate', exact: true }).click();
  const rotation = page.getByRole('dialog', {
    name: 'Rotate Browser application'
  });
  await rotation.getByLabel('Previous-secret overlap (seconds)').fill('60');
  expect(
    (await new AxeBuilder({ page }).include('.secret-dialog').analyze())
      .violations
  ).toEqual([]);
  await page.screenshot({
    path: info.outputPath('key-rotation.png'),
    fullPage: true
  });
  const rotationURL = `**/api/v1/api-keys/${keyId}/rotate`;
  let lostRotationResponse = false;
  let firstRotationId = '';
  await page.route(rotationURL, async (route) => {
    const requestId = route.request().headers()['idempotency-key'];
    if (!lostRotationResponse) {
      lostRotationResponse = true;
      firstRotationId = requestId;
      expect((await route.fetch()).status()).toBe(200);
      await route.abort('failed');
    } else {
      expect(requestId).toBe(firstRotationId);
      await route.continue();
    }
  });
  await rotation
    .getByRole('button', { name: 'Rotate key', exact: true })
    .click();
  await expect(rotation.getByRole('alert')).toBeVisible();
  await expect(
    rotation.getByLabel('Previous-secret overlap (seconds)')
  ).toBeDisabled();
  const rotationResponse = page.waitForResponse(
    (r) =>
      r.url().endsWith(`/api/v1/api-keys/${keyId}/rotate`) &&
      r.request().method() === 'POST'
  );
  await rotation
    .getByRole('button', { name: 'Rotate key', exact: true })
    .click();
  expect((await rotationResponse).status()).toBe(200);
  await page.unroute(rotationURL);
  const replacement = page.getByRole('dialog', {
    name: 'Copy this secret now.'
  });
  await expect(
    replacement.getByText(/previous secret remains valid until/)
  ).toBeVisible();
  await replacement
    .getByRole('button', { name: 'I have saved the key' })
    .click();
  await expect(keyRow.getByText(/Previous .*valid until/)).toBeVisible();

  const notifications = page.locator(
    '[aria-labelledby="notifications-heading"]'
  );
  await notifications.locator('#dest-name').fill('Rotation reminders');
  await notifications
    .getByLabel('Webhook URL', { exact: true })
    .fill('http://127.0.0.1:4190/key-reminders');
  await notifications.locator('#dest-project').selectOption(delegatedProject);
  await notifications
    .getByRole('button', { name: 'Create destination', exact: true })
    .click();
  await expect(
    notifications.getByText('Destination created.', { exact: true })
  ).toBeVisible();
  await notifications.locator('#rule-event').selectOption('key.expiring');
  await notifications.locator('#rule-project').selectOption(delegatedProject);
  await notifications.locator('#rule-name').fill('Browser key reminders');
  await notifications.locator('#rule-subject').selectOption(keyId);
  await notifications
    .locator('#rule-destination')
    .selectOption({ label: 'Rotation reminders' });
  await notifications
    .getByRole('button', { name: 'Create rule', exact: true })
    .click();
  await expect(
    notifications.getByText('Rule created.', { exact: true })
  ).toBeVisible();
  await expect(
    notifications.getByRole('row').filter({ hasText: 'Browser key reminders' })
  ).toContainText('Key expiry or rotation due');
  expect(
    (
      await new AxeBuilder({ page })
        .include('[aria-labelledby="notifications-heading"]')
        .analyze()
    ).violations
  ).toEqual([]);
  await page.evaluate(async () => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
    await new Promise<void>((done) =>
      requestAnimationFrame(() => requestAnimationFrame(() => done()))
    );
  });
  await page.screenshot({
    fullPage: true,

    path: info.outputPath('key-reminders.png')
  });

  await page.goto('/settings/profile');
  await page.getByLabel('Display name').fill('Unsaved owner');
  await changeRemote(page, '/api/v1/profile', 'PATCH', {
    display_name: 'Remote Owner'
  });
  const staleProfile = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/profile') &&
      response.request().method() === 'PATCH'
  );
  await page.getByRole('button', { name: 'Save profile' }).click();
  expect((await staleProfile).status()).toBe(412);
  await expect(page.getByText('This item changed elsewhere.')).toBeVisible();
  await expect(page.getByLabel('Display name')).toHaveValue('Unsaved owner');
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByLabel('Display name')).toHaveValue('Remote Owner');
  await page.getByLabel('Display name').fill('Updated Owner');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(
    page.getByRole('button', { name: 'Open account menu' })
  ).toContainText('Updated Owner');
  // Two pages share the actual session and CSRF cookies. A rotated password
  // session must refresh its sibling before that sibling sends a mutation.
  const sibling = await page.context().newPage();
  await sibling.goto('/settings/profile');
  await expect(sibling.getByLabel('Display name')).toHaveValue('Updated Owner');
  const siblingVerified = sibling.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/sessions/current') &&
      response.status() === 200
  );
  await page.getByLabel('Current password', { exact: true }).fill(password);
  await page.getByLabel('New password', { exact: true }).fill(rotatedPassword);
  await page.getByLabel('Confirm new password').fill(rotatedPassword);
  await page
    .getByRole('button', { name: 'Change password', exact: true })
    .click();
  await siblingVerified;
  await sibling.getByLabel('Display name').fill('Owner from sibling');
  const siblingWrite = sibling.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/profile') &&
      response.request().method() === 'PATCH'
  );
  await sibling.getByRole('button', { name: 'Save profile' }).click();
  // Rotation also changes the profile ETag. CSRF must pass, while the
  // existing edit-conflict guard still requires an explicit reload.
  expect((await siblingWrite).status()).toBe(412);
  await expect(sibling.getByText('This item changed elsewhere.')).toBeVisible();
  await expect(sibling.getByLabel('Display name')).toHaveValue(
    'Owner from sibling'
  );
  await sibling.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(sibling.getByLabel('Display name')).toHaveValue('Updated Owner');
  await sibling.getByLabel('Display name').fill('Owner from sibling');
  const refreshedWrite = sibling.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/profile') &&
      response.request().method() === 'PATCH'
  );
  await sibling.getByRole('button', { name: 'Save profile' }).click();
  expect((await refreshedWrite).status()).toBe(200);
  await expect(
    sibling.getByText('Last session verification', { exact: false })
  ).toBeVisible();
  await expect(sibling.getByText(/Chrome on/)).toBeVisible();
  // Restore the shared journey credential through the real UI, also exercising
  // rotation in the other direction before cross-tab sign-out.
  await sibling
    .getByLabel('Current password', { exact: true })
    .fill(rotatedPassword);
  await sibling.getByLabel('New password', { exact: true }).fill(password);
  await sibling.getByLabel('Confirm new password').fill(password);
  await sibling
    .getByRole('button', { name: 'Change password', exact: true })
    .click();
  await expect(
    sibling.getByText(
      'Password changed. All previous sessions were revoked and this browser was rotated.'
    )
  ).toBeVisible();
  await signOut(sibling);
  await expect(page).toHaveURL(/\/login/);
  await signIn(page, 'owner@example.com');
  await expect(
    page.getByRole('heading', { name: 'Personal profile' })
  ).toBeVisible();
  await expect(sibling).not.toHaveURL(/\/settings\/profile/);
  await sibling.close();

  await page.goto('/settings');
  await page
    .getByLabel('Installation name', { exact: true })
    .fill('Operator test installation');
  await page.getByLabel('Logo', { exact: true }).setInputFiles({
    name: 'operator-logo.png',
    mimeType: 'image/png',
    buffer: Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGMwbPr/HwAFTAKyh13odwAAAABJRU5ErkJggg==',
      'base64'
    )
  });
  await page
    .getByRole('button', { name: 'Save identity', exact: true })
    .click();
  await expect(
    page.getByText('Installation branding saved.', { exact: true })
  ).toBeVisible();
  await page.locator('.installation-switcher summary').click();
  await page.getByLabel('Bookmark name').fill('Second installation');
  await page
    .getByLabel('Installation origin')
    .fill('https://second.example.com');
  await page.getByRole('button', { name: 'Add bookmark', exact: true }).click();
  await expect(
    page.getByRole('option', {
      name: 'Second installation — https://second.example.com'
    })
  ).toHaveCount(1);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('operator-identity.png'),
    fullPage: false
  });
  await page.keyboard.press('Escape');
  let forwardedCookie: string | undefined;
  await page.route('https://second.example.com/**', async (route) => {
    forwardedCookie = (await route.request().allHeaders()).cookie;
    await route.fulfill({
      contentType: 'text/html',
      body: '<!doctype html><html lang="en"><title>Second installation</title><h1>Second installation sign-in</h1></html>'
    });
  });
  const firstOrigin = new URL(page.url()).origin;
  await page.locator('.installation-switcher summary').click();
  await page
    .getByLabel('Switch installation', { exact: true })
    .selectOption('https://second.example.com');
  await expect(page).toHaveURL('https://second.example.com/overview');
  expect(forwardedCookie).toBeUndefined();
  expect(
    await page.evaluate(() => localStorage.getItem('olp.installations.v1'))
  ).toBeNull();
  await page.goto(`${firstOrigin}/settings`);
  await expect(
    page.getByLabel('Installation name', { exact: true })
  ).toHaveValue('Operator test installation');
  const networkRestricted = await page.evaluate(
    async () =>
      (await (await fetch('/api/v1/auth/capabilities')).json())
        .management_network_restricted
  );
  if (networkRestricted) {
    await expect(
      page.getByText('Management access is restricted to client networks', {
        exact: false
      })
    ).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  }
  const retention = page
    .locator('.setting-row')
    .filter({ has: page.locator('[id="setting-retention.audit_days"]') });
  await retention.locator('input').fill('180');
  await changeRemote(page, '/api/v1/settings/retention.audit_days', 'PUT', {
    value: '190'
  });
  const staleSetting = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/settings/retention.audit_days') &&
      response.request().method() === 'PUT'
  );
  await retention.getByRole('button', { name: 'Save', exact: true }).click();
  expect((await staleSetting).status()).toBe(412);
  await expect(retention.locator('input')).toHaveValue('180');
  await page
    .getByRole('button', {
      name: 'Discard this edit and reload the current setting'
    })
    .click();
  await expect(retention.locator('input')).toHaveValue('190');
  await expect(
    page.getByRole('button', {
      name: 'Discard this edit and reload the current setting'
    })
  ).toHaveCount(0);
  await retention.locator('input').fill('180');
  await retention.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(
    retention.getByRole('button', { name: 'Save', exact: true })
  ).toBeDisabled();
  await page.screenshot({
    path: info.outputPath('settings.png'),
    fullPage: true
  });
  await page.goto('/audit');
  await expect(page.getByText('api_key.create', { exact: true })).toBeVisible();
  await page.screenshot({ path: info.outputPath('audit.png'), fullPage: true });

  await page.goto('/access');
  await page.getByRole('button', { name: 'OIDC', exact: true }).click();
  await page.getByLabel('Expected issuer').fill('http://127.0.0.1:4186');
  await page
    .getByLabel('Discovery URL')
    .fill('http://127.0.0.1:4186/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('browser-client');
  await page.getByLabel('Client secret').fill('write-only-browser-secret');
  await page.getByLabel('Enabled', { exact: true }).check();
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration validated and enabled.')
  ).toBeVisible();
  await page.getByLabel('Client ID').fill('unsaved-client');
  await changeRemote(page, '/api/v1/oidc/configuration', 'PUT', {
    issuer: 'http://127.0.0.1:4186',
    discovery_url: 'http://127.0.0.1:4186/.well-known/openid-configuration',
    client_id: 'remote-browser-client',
    enabled: true,
    default_role: 'viewer'
  });
  const staleOidc = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v1/oidc/configuration') &&
      response.request().method() === 'PUT'
  );
  await page.getByRole('button', { name: 'Save and validate' }).click();
  expect((await staleOidc).status()).toBe(412);
  await expect(page.getByText('This item changed elsewhere.')).toBeVisible();
  await expect(page.getByLabel('Client ID')).toHaveValue('unsaved-client');
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByLabel('Client ID')).toHaveValue(
    'remote-browser-client'
  );
  await page.getByLabel('Client ID').fill('browser-client');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration validated and enabled.')
  ).toBeVisible();
  for (const returnTo of [
    '/\t/evil.example',
    '/\r/evil.example',
    '/\n/evil.example'
  ]) {
    const rejected = await page.evaluate(async (returnTo) => {
      // Chromium strips these controls and would navigate to another origin.
      const destination = new URL(returnTo, location.href);
      const post = await fetch('/api/v1/oidc/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ return_to: returnTo })
      });
      const get = await fetch(
        '/api/v1/oidc/login?return_to=' + encodeURIComponent(returnTo),
        {
          redirect: 'manual'
        }
      );
      return {
        crossOrigin: destination.origin !== location.origin,
        post: post.status,
        get: get.status
      };
    }, returnTo);
    expect(rejected).toEqual({ crossOrigin: true, post: 422, get: 422 });
  }
  await signOut(page);
  await page.getByRole('link', { name: /single sign-on|OIDC/ }).click();
  await expect(
    page.getByRole('heading', { name: viewerLanding })
  ).toBeVisible();
  await page.goto('/settings/profile');
  await expect(page.getByLabel('Display name')).toHaveValue('SSO Member');
  await expect(page.getByText('Updated Owner', { exact: true })).toHaveCount(0);
  await page.screenshot({
    path: info.outputPath('oidc-profile.png'),
    fullPage: true
  });
  await signOut(page);
  await signIn(page, 'viewer@example.com');
  await expect(
    page.getByRole('heading', { name: viewerLanding })
  ).toBeVisible();
  await page.goto('/api-keys');
  await expect(page.getByRole('link', { name: 'Create key' })).toHaveCount(0);
  expect(failures).toEqual([]);
});

test('capabilities and passive verification recover without losing loaded content', async ({
  page
}, info) => {
  let capabilitiesFailed = false;
  await page.route('**/api/v1/auth/capabilities', async (route) => {
    if (!capabilitiesFailed) {
      capabilitiesFailed = true;
      await route.fulfill({
        status: 503,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          status: 503,
          title: 'Sign-in discovery unavailable'
        })
      });
    } else await route.continue();
  });
  await page.goto('/login');
  await expect(page.getByRole('alert')).toContainText(
    'Sign-in options could not be loaded'
  );
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await signIn(page, 'owner@example.com');
  await expect(page.getByRole('heading', { name: ownerLanding })).toBeVisible();
  await page.goto('/settings/profile');
  await expect(page.getByLabel('Display name')).toHaveValue(
    'Owner from sibling'
  );
  let unavailable = true;
  await page.route('**/api/v1/sessions/current', async (route) => {
    if (unavailable && route.request().method() === 'GET')
      await route.fulfill({
        status: 503,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          status: 503,
          title: 'Session service unavailable'
        })
      });
    else await route.continue();
  });
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  const banner = page
    .getByRole('alert')
    .filter({ hasText: 'Session verification unavailable' });
  await expect(banner).toBeVisible();
  await expect(page.getByLabel('Display name')).toHaveValue(
    'Owner from sibling'
  );
  let writes = 0;
  page.on('request', (request) => {
    if (
      request.url().endsWith('/api/v1/profile') &&
      request.method() === 'PATCH'
    )
      writes++;
  });
  await page.getByLabel('Display name').fill('Retained edit');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.locator('#profile-error')).toHaveText(
    'Session service unavailable'
  );
  expect(writes).toBe(0);
  await page.screenshot({
    path: info.outputPath('verification-degraded.png'),
    fullPage: true
  });
  unavailable = false;
  await banner.getByRole('button', { name: 'Retry' }).click();
  await expect(banner).toHaveCount(0);
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText('Profile updated.')).toBeVisible();
  expect(writes).toBe(1);
  // A CSRF rejection is an explicit retry, never an automatic replay.
  await page.route('**/api/v1/profile', async (route) => {
    if (route.request().method() === 'PATCH') {
      await route.fulfill({
        status: 403,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          status: 403,
          title: 'CSRF invalid',
          type: 'https://openllmproxy.dev/problems/csrf_invalid'
        })
      });
      await page.unroute('**/api/v1/profile');
    } else await route.continue();
  });
  await page.getByLabel('Display name').fill('Explicit retry');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText(/Review your changes and retry/)).toBeVisible();
  expect(writes).toBe(2);
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText('Profile updated.')).toBeVisible();
  expect(writes).toBe(3);
  await signOut(page);
});
