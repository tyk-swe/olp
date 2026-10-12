import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { listBudgetGroups, updateBudgetGroup, type BudgetGroup } from './api';
import BudgetGroupsProbe from './test/BudgetGroupsProbe.svelte';
import { budgetGroupKeys } from './budgetGroupKeys';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true, user: { access_scope: 'global' } })
}));
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({ limitsEnforced: true })
}));
vi.mock('$lib/features/access/projects/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/projects/api')>()),
  listProjectMemberships: vi.fn(async () => [])
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listBudgetGroups: vi.fn(),
  updateBudgetGroup: vi.fn()
}));
const group = {
  id: 'group-1',
  etag: 'group-etag-1',
  name: 'First group',
  project_id: null,
  daily_cost_limit: '100',
  monthly_cost_limit: '1000',
  budget: {
    unpriced_attempts: 0,
    daily: { accrued: '0', limit: '100', reset_at: '2026-10-12T00:00:00Z' },
    monthly: { accrued: '0', limit: '1000', reset_at: '2026-11-01T00:00:00Z' }
  }
} as BudgetGroup;
const other = {
  ...group,
  id: 'group-2',
  etag: 'group-etag-2',
  name: 'Second group'
};
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listBudgetGroups).mockResolvedValue([group, other]);
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});
async function render() {
  component = mount(BudgetGroupsProbe, { target: host, props: { client } });
  await vi.waitFor(() => expect(host.textContent).toContain('Second group'));
  flushSync();
}
function button(text: string) {
  return [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === text
  )!;
}
function field(value: string) {
  const input = host.querySelector<HTMLInputElement>(
    '[aria-label="Group name"]'
  )!;
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}
it('keeps the submitted group selected until its delayed save completes', async () => {
  let finish!: () => void;
  vi.mocked(updateBudgetGroup).mockImplementation(
    () =>
      new Promise((resolve) => {
        finish = () => resolve({ etag: 'saved' });
      })
  );
  await render();
  button('Edit').click();
  flushSync();
  field('Submitted group');
  button('Save').click();
  flushSync();
  const otherEdit = button('Edit');
  expect(otherEdit.disabled).toBe(true);
  otherEdit.click();
  flushSync();
  expect(
    host.querySelector<HTMLInputElement>('[aria-label="Group name"]')!.value
  ).toBe('Submitted group');
  finish();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Budget group updated.')
  );
  const secondRow = [...host.querySelectorAll('tbody tr')].find((row) =>
    row.textContent?.includes('Second group')
  )!;
  secondRow.querySelector<HTMLButtonElement>('button')!.click();
  flushSync();
  field('New second-group draft');
  expect(
    host.querySelector<HTMLInputElement>('[aria-label="Group name"]')!.value
  ).toBe('New second-group draft');
});

it('retains the original group ETag and draft body after an ambiguous save and inventory refresh', async () => {
  vi.mocked(updateBudgetGroup)
    .mockRejectedValueOnce(new TypeError('Response lost'))
    .mockResolvedValueOnce({ etag: 'saved' });
  await render();
  button('Edit').click();
  flushSync();
  field('Same group draft');
  button('Save').click();
  await vi.waitFor(() => expect(host.textContent).toContain('Response lost'));
  client.setQueryData(budgetGroupKeys.list(), [
    { ...group, etag: 'new-inventory-etag' },
    other
  ]);
  vi.spyOn(client, 'invalidateQueries').mockRejectedValueOnce(
    new Error('Refresh unavailable')
  );
  flushSync();
  button('Save').click();
  await vi.waitFor(() => expect(updateBudgetGroup).toHaveBeenCalledTimes(2));
  expect(vi.mocked(updateBudgetGroup).mock.calls[1]).toEqual(
    vi.mocked(updateBudgetGroup).mock.calls[0]
  );
  expect(vi.mocked(updateBudgetGroup).mock.calls[1][0].etag).toBe(group.etag);
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'Budget group updated, but refreshing failed.'
    )
  );
  expect(host.textContent).toContain('Budget group updated.');
  expect(host.querySelector('[aria-label="Group name"]')).toBeNull();
});
