import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/management/test/transport';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import BudgetGroupsPanel from './BudgetGroupsPanel.svelte';

afterEach(() => vi.unstubAllGlobals());

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
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(BudgetGroupsPanel, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );
  await vi.waitFor(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  flushSync();
  const name = target.querySelector<HTMLInputElement>(
    '[aria-label="Group name"]'
  );
  expect(name).not.toBeNull();
  if (!name) return;
  name.value = 'Unsaved group name';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  button('Retire budget group')?.click();
  flushSync();
  button('Confirm retire budget group')?.click();
  await vi.waitFor(() =>
    expect(retired).toHaveBeenCalledExactlyOnceWith(
      '/api/v1/budget-groups/retiring'
    )
  );
  await vi.waitFor(() =>
    expect(target.querySelectorAll('tbody tr')).toHaveLength(1)
  );
  expect(
    target.querySelector<HTMLInputElement>('[aria-label="Group name"]')?.value
  ).toBe('Unsaved group name');
  button('Save')?.click();
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  expect(new URL(saved.mock.calls[0][0].url).pathname).toBe(
    '/api/v1/budget-groups/editing'
  );
  expect(await saved.mock.calls[0][0].json()).toHaveProperty(
    'name',
    'Unsaved group name'
  );
});
