import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue
} from '#test/mount.js';
import BudgetGroupsPanel from './BudgetGroupsPanel.svelte';

it('keeps an edited budget group when another group is retired', async () => {
  const retired = vi.fn();
  const saved = vi.fn<(request: Request) => void>();
  const groups = ['editing', 'retiring'].map((id) => ({
    id,
    name: id,
    etag: id,
    daily_cost_limit: '10',
    budget: {
      cost_basis: 'cost',
      enforcement_active: true,
      unpriced_attempts: 0,
      daily: { accrued: '0', limit: '10', reset_at: '2026-01-01T00:00:00Z' },
      monthly: { accrued: '0', limit: null, reset_at: '2026-02-01T00:00:00Z' }
    }
  }));
  serve(
    (request) => {
      if (request.method === 'DELETE') {
        retired(new URL(request.url).pathname);
        return new Response(null, { status: 204 });
      }
      if (request.method === 'PATCH') {
        saved(request);
        return json({ etag: 'saved' });
      }
      if (new URL(request.url).pathname === '/api/v1/auth/capabilities')
        return json({ local_login_enabled: true, oidc_login_enabled: false });
      if (new URL(request.url).pathname === '/api/v1/budget-groups')
        return json({
          items: retired.mock.calls.length ? groups.slice(0, 1) : groups
        });
      return json({ items: [] });
    },
    ['read', 'keys', 'configure']
  );
  const { target } = mountWithState(BudgetGroupsPanel);
  const button = (label: string) => findButton(target, label);
  await eventually(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  flushSync();
  const name = target.querySelector<HTMLInputElement>(
    '[aria-label="Group name"]'
  );
  expect(name).not.toBeNull();
  if (!name) return;
  setFieldValue(name, 'Unsaved group name');
  button('Retire budget group')?.click();
  flushSync();
  button('Confirm retire budget group')?.click();
  await eventually(() =>
    expect(retired).toHaveBeenCalledExactlyOnceWith(
      '/api/v1/budget-groups/retiring'
    )
  );
  await eventually(() =>
    expect(target.querySelectorAll('tbody tr')).toHaveLength(1)
  );
  expect(
    target.querySelector<HTMLInputElement>('[aria-label="Group name"]')?.value
  ).toBe('Unsaved group name');
  button('Save')?.click();
  await eventually(() => expect(saved).toHaveBeenCalledOnce());
  expect(new URL(saved.mock.calls[0][0].url).pathname).toBe(
    '/api/v1/budget-groups/editing'
  );
  expect(await saved.mock.calls[0][0].json()).toHaveProperty(
    'name',
    'Unsaved group name'
  );
});

it.each(['create', 'edit'] as const)(
  'saves the selected budget basis on %s',
  async (mode) => {
    const saved = vi.fn<(request: Request) => void>();
    const group = {
      id: 'group',
      name: 'Existing',
      etag: 'version',
      cost_basis: 'cost',
      daily_cost_limit: '10',
      budget: {
        cost_basis: 'cost',
        enforcement_active: true,
        unpriced_attempts: 0,
        daily: { accrued: '0', limit: '10', reset_at: '2026-01-01T00:00:00Z' },
        monthly: { accrued: '0', limit: null, reset_at: '2026-02-01T00:00:00Z' }
      }
    };
    serve(
      (request) => {
        if (request.method === 'POST' || request.method === 'PATCH') {
          saved(request);
          return json({ id: 'group', etag: 'saved' });
        }
        if (new URL(request.url).pathname === '/api/v1/budget-groups')
          return json({ items: [group] });
        return json({ items: [] });
      },
      ['read', 'keys', 'configure']
    );
    mountWithState(BudgetGroupsPanel, undefined, { attach: true });
    const button = (label: string) => findButton(document, label)!;
    await eventually(() => expect(button('Edit')).toBeDefined());
    button(mode === 'create' ? 'New group' : 'Edit').click();
    flushSync();
    if (mode === 'create') {
      setFieldValue(findControl(document, 'Group name'), 'Created');
      setFieldValue(findControl(document, 'Daily cost limit'), '10');
    }
    const select = [...document.querySelectorAll('select')].find((control) =>
      control.querySelector('option[value="charge"]')
    )!;
    setFieldValue(select, 'charge', 'change');
    button(mode === 'create' ? 'Create group' : 'Save').click();
    await eventually(() => expect(saved).toHaveBeenCalledOnce());
    expect(await saved.mock.calls[0][0].json()).toHaveProperty(
      'cost_basis',
      'charge'
    );
  }
);

it('reports each refused limit at the input that holds it', async () => {
  const written = vi.fn();
  const group = {
    id: 'group',
    name: 'Existing',
    etag: 'version',
    daily_cost_limit: '10',
    budget: {
      cost_basis: 'cost',
      enforcement_active: true,
      unpriced_attempts: 0,
      daily: { accrued: '0', limit: '10', reset_at: '2026-01-01T00:00:00Z' },
      monthly: { accrued: '0', limit: null, reset_at: '2026-02-01T00:00:00Z' }
    }
  };
  serve(
    (request) => {
      if (request.method !== 'GET') {
        written();
        const field = request.method === 'POST' ? 'name' : 'weekly_cost_limit';
        return json(
          {
            type: 'about:blank',
            title: 'Unprocessable Content',
            status: 422,
            detail: 'The request was refused.',
            errors: {
              [field]: [{ code: 'refused', message: `Refused ${field}.` }]
            }
          },
          422
        );
      }
      if (new URL(request.url).pathname === '/api/v1/budget-groups')
        return json({ items: [group] });
      return json({ items: [] });
    },
    ['read', 'keys', 'configure']
  );
  mountWithState(BudgetGroupsPanel, undefined, { attach: true });
  const button = (label: string) => findButton(document, label)!;
  const inRow = (label: string) =>
    document.querySelector<HTMLInputElement>(`[aria-label="${label}"]`)!;
  await eventually(() => expect(button('Edit')).toBeDefined());

  button('Edit').click();
  flushSync();
  setFieldValue(inRow('Monthly cost limit'), 'ten');
  button('Save').click();
  await eventually(() =>
    expect(reportedError(inRow('Monthly cost limit'))).toContain(
      'positive decimals'
    )
  );
  expect(written).not.toHaveBeenCalled();
  setFieldValue(inRow('Monthly cost limit'), '100');
  button('Save').click();
  await eventually(() =>
    expect(reportedError(inRow('Weekly cost limit'))).toBe(
      'Refused weekly_cost_limit.'
    )
  );
  button('Cancel').click();

  button('New group').click();
  flushSync();
  const inDialog = (label: string) => findControl(document, label);
  setFieldValue(inDialog('Group name'), 'Created');
  setFieldValue(inDialog('requests per minute'), '  ');
  button('Create group').click();
  await eventually(() =>
    expect(
      document.querySelector('dialog [role="alert"]')?.textContent
    ).toContain('Set a limit or reference a project template.')
  );
  expect(written).toHaveBeenCalledOnce();

  setFieldValue(inDialog('requests per minute'), '1.5');
  setFieldValue(inDialog('Limit template'), 'shared');
  button('Create group').click();
  await eventually(() =>
    expect(reportedError(inDialog('requests per minute'))).toContain(
      'positive whole numbers'
    )
  );
  expect(reportedError(inDialog('Limit template'))).toBe(
    'Choose a project and a valid template name.'
  );
  expect(written).toHaveBeenCalledOnce();

  setFieldValue(inDialog('requests per minute'), '2');
  setFieldValue(inDialog('Limit template'), '');
  button('Create group').click();
  await eventually(() =>
    expect(reportedError(inDialog('Group name'))).toBe('Refused name.')
  );
});
