import AxeBuilder from '@axe-core/playwright';
import { test, expect, type Page } from '../playwright';

test('creates a TypeSafe guardrail without endpoint egress acknowledgement', async ({
  page
}) => {
  const operations = ['read', 'configure', 'playground', 'keys', 'settings'];
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    let body: unknown = { items: [] };
    if (path === '/api/v1/sessions/current') {
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
    }
    if (path === '/api/v1/projects')
      body = { items: [{ id: 'project', name: 'Project' }] };
    if (path === '/api/v1/guardrails' && request.method() === 'POST') {
      const definition = request.postDataJSON();
      expect(definition).toMatchObject({
        name: 'Relevance pruning',
        project_id: 'project',
        type: 'typesafe',
        configuration: { route: 'relevance-route' }
      });
      expect(definition.configuration).not.toHaveProperty('content_egress');
      body = {
        ...definition,
        id: 'typesafe',
        revision_id: 'revision',
        revision: 1,
        etag: 'etag'
      };
    }
    await route.fulfill({
      json: body,
      headers: { 'Cache-Control': 'no-store' }
    });
  });
  await page.goto('/guardrails');
  const form = page
    .locator('section.panel')
    .filter({ has: page.getByRole('heading', { name: 'Definitions' }) })
    .locator('form');
  await form.getByLabel('Name', { exact: true }).fill('Relevance pruning');
  await form.getByRole('combobox', { name: 'Project' }).selectOption('project');
  await form.getByRole('combobox', { name: 'Type' }).selectOption('typesafe');
  await form
    .getByLabel('Configuration (JSON)')
    .fill('{"route":"relevance-route"}');
  await expect(
    form.getByText(
      'I acknowledge that this guardrail sends inspected content',
      { exact: false }
    )
  ).toHaveCount(0);
  await form.getByRole('button', { name: 'Save draft' }).click();
  await expect(
    page.getByText('Draft saved. Activate its revision when it is ready.')
  ).toBeVisible();
});

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

async function delayFirstGuardrailSave(
  page: Page,
  resourcePath: string,
  newDestination: boolean
) {
  const pending = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const original = {
    id: 'original',
    policy_id: 'original',
    name: 'Original',
    project_id: 'project',
    type: 'builtin.keywords',
    configuration: { words: ['restricted'] },
    timeout_ms: 1000,
    on_error: 'fail_closed',
    entries: [],
    revision: 1,
    revision_id: 'original-revision',
    etag: 'original-etag'
  };
  const other = {
    ...original,
    id: 'other',
    policy_id: 'other',
    name: 'Other',
    etag: 'other-etag'
  };
  const writes: {
    method: string;
    path: string;
    name: string;
    etag: string | undefined;
  }[] = [];
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const operations = ['read', 'configure', 'playground', 'keys', 'settings'];
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
    if (path === '/api/v1/projects')
      body = { items: [{ id: 'project', name: 'Project' }] };
    if (path === `/api/v1/${resourcePath}`) body = { items: [original, other] };
    if (['POST', 'PUT'].includes(request.method())) {
      const submitted = request.postDataJSON();
      writes.push({
        method: request.method(),
        path,
        name: submitted.name,
        etag: request.headers()['if-match']
      });
      if (writes.length === 1) {
        pending.resolve();
        await release.promise;
      }
      const id = path.endsWith('/original')
        ? 'original'
        : newDestination
          ? 'created'
          : 'other';
      body = {
        ...original,
        ...submitted,
        id,
        policy_id: id,
        etag: `${id}-saved-etag`,
        revision: 2
      };
    }
    await route.fulfill({
      json: body,
      headers: { 'Cache-Control': 'no-store' }
    });
  });
  return { pending, release, writes };
}

for (const editor of [
  {
    tab: 'Definitions',
    path: 'guardrails',
    create: 'New definition',
    save: 'Save draft'
  },
  {
    tab: 'Policies',
    path: 'guardrail-policies',
    create: 'New policy',
    save: 'Save policy draft'
  }
]) {
  for (const destination of [
    'new',
    'existing',
    'same',
    'round-trip',
    'new-from-new'
  ]) {
    test(`late ${editor.tab} save preserves the ${destination} editor`, async ({
      page
    }) => {
      const newSource = destination === 'new-from-new';
      const newDestination = destination === 'new' || newSource;
      const sameDestination =
        destination === 'same' || destination === 'round-trip';
      const existingId = sameDestination ? 'original' : 'other';
      const existingName = sameDestination ? 'Original' : 'Other';
      const destinationPath = newDestination ? '' : `/${existingId}`;
      const existingEtag = sameDestination
        ? '"original-saved-etag"'
        : '"other-etag"';
      const { pending, release, writes } = await delayFirstGuardrailSave(
        page,
        editor.path,
        newDestination
      );
      try {
        await page.goto('/guardrails');
        await page
          .getByRole('button', { name: editor.tab, exact: true })
          .click();
        await page
          .getByRole('button', { name: 'Original', exact: true })
          .click();
        if (newSource)
          await page
            .getByRole('button', { name: editor.create, exact: true })
            .click();
        const name = page.getByRole('textbox', { name: 'Name', exact: true });
        const save = page.getByRole('button', {
          name: editor.save,
          exact: true
        });
        await name.fill('Original submitted');
        await save.click();
        await pending.promise;
        if (destination === 'round-trip')
          await page
            .getByRole('button', { name: 'Other', exact: true })
            .click();
        await page
          .getByRole('button', {
            name: newDestination ? editor.create : existingName,
            exact: true
          })
          .click();
        await name.fill('Current draft');
        release.resolve();
        await expect(save).toBeEnabled();
        await expect(page.getByRole('status')).toHaveCount(0);
        await expect(name).toHaveValue('Current draft');
        await save.click();
        await expect(save).toBeEnabled();
        expect(writes).toEqual([
          {
            method: newSource ? 'POST' : 'PUT',
            path: `/api/v1/${editor.path}${newSource ? '' : '/original'}`,
            name: 'Original submitted',
            etag: newSource ? undefined : '"original-etag"'
          },
          {
            method: newDestination ? 'POST' : 'PUT',
            path: `/api/v1/${editor.path}${destinationPath}`,
            name: 'Current draft',
            etag: newDestination ? undefined : existingEtag
          }
        ]);
        await name.fill('Current draft again');
        await save.click();
        await expect(save).toBeEnabled();
        const id = newDestination ? 'created' : existingId;
        expect(writes[2]).toEqual({
          method: 'PUT',
          path: `/api/v1/${editor.path}/${id}`,
          name: 'Current draft again',
          etag: `"${id}-saved-etag"`
        });
      } finally {
        release.resolve();
      }
    });
  }
}
