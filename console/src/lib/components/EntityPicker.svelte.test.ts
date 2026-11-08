import { expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../tests/unit/support/StateHarness.svelte';
import PickerProbe from './test/PickerProbe.svelte';

const person = {
  id: '00000000-0000-4000-8000-000000000002',
  email: 'dev@example.test',
  display_name: 'Dana Developer',
  role: 'developer',
  access_scope: 'global',
  active: true,
  etag: 'etag',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

function render(operations: Parameters<typeof serve>[1]) {
  serve(
    (request) =>
      new URL(request.url).pathname === '/api/v1/users'
        ? json({ items: [person] })
        : json({ items: [] }),
    operations
  );
  const picked = vi.fn<(value: string) => void>();
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(PickerProbe, {
      target,
      props: { picked },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    target.remove();
  });
  return { target, picked };
}

it('lists people by name and hands back the chosen ID', async () => {
  const { target, picked } = render(['access_read']);

  const select = await vi.waitFor(() => {
    const found = target.querySelector<HTMLSelectElement>('select#probe');
    expect(found?.textContent).toContain('Dana Developer');
    return found;
  });
  if (!select) throw new Error('missing picker');
  select.value = person.id;
  select.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();

  expect(picked).toHaveBeenCalledExactlyOnceWith(person.id);
});

it('falls back to typing an ID when the role cannot list people', async () => {
  const { target } = render([]);

  await vi.waitFor(() =>
    expect(target.querySelector('input#probe')).not.toBeNull()
  );
  expect(target.querySelector('select#probe')).toBeNull();
});
