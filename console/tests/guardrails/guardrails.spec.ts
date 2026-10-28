import AxeBuilder from '@axe-core/playwright';
import { test, expect } from '../playwright';

test('paginated guardrail editors and playground support keyboard access without storing content', async ({
  page
}) => {
  const operations = ['read', 'configure', 'playground', 'keys', 'settings'];
  const definition = {
    id: 'guard',
    project_id: 'project',
    name: 'Private words',
    type: 'builtin.keywords',
    revision_id: 'revision',
    revision: 1,
    configuration: { words: ['restricted'] },
    policy: { rules: [] },
    timeout_ms: 1000,
    on_error: 'fail_closed',
    etag: 'etag',
    active_revision_id: 'revision'
  };
  const policy = {
    id: 'policy',
    policy_id: 'policy',
    name: 'Project policy',
    project_id: 'project',
    revision_id: 'policy-revision',
    revision: 1,
    entries: [],
    etag: 'etag',
    active_revision_id: 'policy-revision'
  };
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    let body: unknown = { items: [], next_cursor: null };
    if (path === '/api/v1/sessions/current')
      body = {
        user: {
          id: 'owner',
          email: 'owner@example.test',
          display_name: 'Owner',
          role: 'owner',
          access_scope: 'global',
          operations
        },
        csrf_token: 'test-csrf',
        operations,
        installation_name: 'Guardrail test'
      };
    if (path === '/api/v1/guardrails')
      body =
        url.searchParams.get('cursor') === 'definitions-next'
          ? { items: [definition], next_cursor: null }
          : {
              items: Array.from({ length: 50 }, (_, i) => ({
                ...definition,
                id: `first-${i}`,
                name: `First definition ${i}`,
                revision_id: `first-revision-${i}`
              })),
              next_cursor: 'definitions-next'
            };
    if (path === '/api/v1/guardrail-policies')
      body =
        url.searchParams.get('cursor') === 'policies-next'
          ? { items: [policy], next_cursor: null }
          : {
              items: Array.from({ length: 50 }, (_, i) => ({
                ...policy,
                id: `first-policy-${i}`,
                policy_id: `first-policy-${i}`,
                name: `First policy ${i}`,
                revision_id: `first-policy-revision-${i}`
              })),
              next_cursor: 'policies-next'
            };
    if (path === '/api/v1/projects')
      body =
        url.searchParams.get('cursor') === 'projects-next'
          ? { items: [{ id: 'project', name: 'Project' }], next_cursor: null }
          : {
              items: Array.from({ length: 50 }, (_, i) => ({
                id: `first-project-${i}`,
                name: `First project ${i}`
              })),
              next_cursor: 'projects-next'
            };
    if (
      path === '/api/v1/guardrail-policies/policy' &&
      route.request().method() === 'PUT'
    ) {
      const request = route.request().postDataJSON();
      expect(request.entries[0].guardrail_id).toBe('guard');
      expect(request.entries[0].revision_id).toBe('revision');
      body = { ...policy, entries: request.entries };
    }
    if (
      path === '/api/v1/guardrail-attachments' &&
      route.request().method() === 'POST'
    ) {
      const request = route.request().postDataJSON();
      expect(request.policy_revision_id).toBe('policy-revision');
      expect(request.scope_id).toBe('project');
      body = request;
    }
    if (path === '/api/v1/guardrails/evaluate') {
      const request = route.request().postDataJSON();
      expect(request.policy_revision_id).toBe('policy-revision');
      expect(request.segments[0].text).toBe('private example');
      body = { blocked: false, segments: request.segments, decisions: [] };
    }
    await route.fulfill({
      json: body,
      headers: { 'Cache-Control': 'no-store' }
    });
  });
  await page.goto('/guardrails');
  await expect(
    page.getByRole('heading', { name: 'Guardrails', exact: true })
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Private words', exact: true })
    .click();
  await expect(
    page.getByRole('textbox', { name: 'Name', exact: true })
  ).toHaveValue('Private words');
  await expect(
    page.getByRole('combobox', { name: 'Project', exact: true })
  ).toHaveValue('project');
  for (const name of ['Definitions', 'Policies', 'Attachments', 'Playground']) {
    const tab = page.getByRole('button', { name, exact: true });
    await tab.focus();
    await page.keyboard.press('Enter');
    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21aa'])
      .analyze();
    expect(
      results.violations.map((violation) => ({
        id: violation.id,
        nodes: violation.nodes.map((node) => node.target)
      }))
    ).toEqual([]);
  }
  await page.getByRole('button', { name: 'Policies', exact: true }).click();
  await page
    .getByRole('button', { name: 'Project policy', exact: true })
    .click();
  await page
    .getByRole('button', { name: 'Add guardrail', exact: true })
    .click();
  await page
    .getByRole('combobox', { name: 'Guardrail', exact: true })
    .selectOption('guard');
  await page
    .getByRole('button', { name: 'Save policy draft', exact: true })
    .click();
  await expect(page.getByRole('status')).toHaveText(
    'Policy draft saved with its ordered, pinned revisions.'
  );
  await page.getByRole('button', { name: 'Attachments', exact: true }).click();
  await page
    .getByRole('combobox', { name: 'Policy revision', exact: true })
    .selectOption('policy-revision');
  await page
    .getByRole('textbox', { name: 'Scope identifier', exact: true })
    .fill('project');
  await page
    .getByRole('button', { name: 'Attach policy', exact: true })
    .click();
  await expect(page.getByRole('status')).toHaveText(
    'Policy attached. Inherited policies continue to apply.'
  );
  await page.getByRole('button', { name: 'Playground', exact: true }).click();
  await page
    .getByRole('combobox', { name: 'Policy revision', exact: true })
    .selectOption('policy-revision');
  await page
    .getByRole('textbox', { name: 'Text', exact: true })
    .fill('private example');
  await page.getByRole('button', { name: 'Evaluate', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Allowed', exact: true })
  ).toBeVisible();
  expect(
    await page.evaluate(() =>
      JSON.stringify({ ...localStorage, ...sessionStorage })
    )
  ).not.toContain('private example');
  await page
    .getByRole('button', { name: 'Clear content', exact: true })
    .click();
  await expect(
    page.getByRole('textbox', { name: 'Text', exact: true })
  ).toHaveValue('');
});

for (const inventory of ['guardrails', 'guardrail-policies', 'projects']) {
  test(`incomplete ${inventory} inventory stays pending then reports a continuation failure`, async ({
    page
  }) => {
    const continuation = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    await page.route('**/api/v1/**', async (route) => {
      const url = new URL(route.request().url());
      const operations = [
        'read',
        'configure',
        'playground',
        'keys',
        'settings'
      ];
      if (url.pathname === '/api/v1/sessions/current') {
        await route.fulfill({
          json: {
            user: {
              id: 'owner',
              email: 'owner@example.test',
              display_name: 'Owner',
              role: 'owner',
              access_scope: 'global',
              operations
            },
            csrf_token: 'test-csrf',
            operations,
            installation_name: 'Guardrail test'
          },
          headers: { 'Cache-Control': 'no-store' }
        });
        return;
      }
      if (url.pathname !== `/api/v1/${inventory}`) {
        await route.fulfill({
          json: { items: [], next_cursor: null },
          headers: { 'Cache-Control': 'no-store' }
        });
        return;
      }
      if (!url.searchParams.get('cursor')) {
        await route.fulfill({
          json: {
            items: [
              {
                id: 'partial',
                policy_id: 'partial',
                name: 'Partial inventory entry',
                type: 'builtin.keywords',
                revision: 1,
                revision_id: 'partial-revision'
              }
            ],
            next_cursor: 'next'
          },
          headers: { 'Cache-Control': 'no-store' }
        });
        return;
      }
      continuation.resolve();
      await release.promise;
      await route.fulfill({
        status: 403,
        json: { title: 'Continuation refused', status: 403 },
        headers: { 'Cache-Control': 'no-store' }
      });
    });
    try {
      await page.goto('/guardrails');
      await continuation.promise;
      if (inventory === 'guardrail-policies') {
        await page
          .getByRole('button', { name: 'Policies', exact: true })
          .click();
      }
      await expect(page.getByRole('status')).toHaveText(
        'Loading guardrail inventory…'
      );
      await expect(
        page.getByText('Partial inventory entry', { exact: true })
      ).toHaveCount(0);
      release.resolve();
      await expect(page.getByRole('alert')).toHaveText(
        'Guardrails could not be loaded. Reload to try again.'
      );
      await expect(page.getByRole('status')).toHaveCount(0);
      await expect(
        page.getByText('Partial inventory entry', { exact: true })
      ).toHaveCount(0);
    } finally {
      release.resolve();
    }
  });
}
