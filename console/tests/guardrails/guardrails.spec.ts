import AxeBuilder from '@axe-core/playwright';
import { test, expect } from '../playwright';

test('guardrail editors and playground support keyboard access without storing content', async ({
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
    const path = new URL(route.request().url()).pathname;
    let body: unknown = { items: [] };
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
    if (path === '/api/v1/guardrails') body = { items: [definition] };
    if (path === '/api/v1/guardrail-policies') body = { items: [policy] };
    if (path === '/api/v1/projects')
      body = { items: [{ id: 'project', name: 'Project' }] };
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
  await page
    .getByRole('combobox', { name: 'Policy revision', exact: true })
    .selectOption('policy-revision');
  await page
    .getByRole('textbox', { name: 'Text', exact: true })
    .fill('private example');
  await page.getByRole('button', { name: 'Evaluate', exact: true }).click();
  await expect(
    page.getByRole('button', { name: 'Evaluate', exact: true })
  ).toBeEnabled();
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
