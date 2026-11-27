import { expect, it, vi } from 'vitest';
import { personOptions } from '#lib/features/identity/index.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, setFieldValue } from '#test/mount.js';
import EntityPicker from './EntityPicker.svelte';

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
  const { target } = mountWithState(
    EntityPicker,
    {
      id: 'probe',
      source: personOptions,
      emptyLabel: 'Anyone',
      onchange: picked
    },
    { attach: true }
  );
  return { target, picked };
}

it('lists people by name and hands back the chosen ID', async () => {
  const { target, picked } = render(['access_read']);

  const select = await eventually(() => {
    const found = target.querySelector<HTMLSelectElement>('select#probe');
    expect(found?.textContent).toContain('Dana Developer');
    return found;
  });
  if (!select) throw new Error('missing picker');
  setFieldValue(select, person.id, 'change');

  expect(picked).toHaveBeenCalledExactlyOnceWith(person.id);
});

it('falls back to typing an ID when the role cannot list people', async () => {
  const { target } = render([]);

  await eventually(() =>
    expect(target.querySelector('input#probe')).not.toBeNull()
  );
  expect(target.querySelector('select#probe')).toBeNull();
});
