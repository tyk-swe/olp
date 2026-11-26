import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState } from '#test/mount.js';
import MembersPanel from './MembersPanel.svelte';

const member = {
  id: '00000000-0000-4000-8000-000000000002',
  email: 'dev@example.test',
  display_name: 'Dana Developer',
  role: 'developer',
  access_scope: 'global',
  active: true,
  etag: 'member-etag',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

it('changes a member role only after the change is confirmed', async () => {
  const patched = vi.fn<(body: unknown) => void>();
  const write = Promise.withResolvers<Response>();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'PATCH') {
        patched(await request.json());
        return write.promise;
      }
      if (path === '/api/v1/auth/capabilities')
        return json({ local_login_enabled: true, oidc_login_enabled: false });
      if (path === '/api/v1/users')
        return json({
          items: [
            member,
            {
              ...member,
              id: 'other-member',
              display_name: 'Other member',
              active: false
            }
          ]
        });
      return json({ items: [] });
    },
    ['read', 'access']
  );
  const { target } = mountWithState(MembersPanel, undefined, { attach: true });
  const button = (label: string) => findButton(target, label);
  const chooseOperator = () => {
    const radio = target.querySelector<HTMLInputElement>(
      'input[name="role"][value="operator"]'
    );
    expect(radio).not.toBeNull();
    radio?.click();
    flushSync();
  };

  await eventually(() => expect(button('Change access')).toBeDefined());
  button('Change access')?.click();
  flushSync();
  chooseOperator();
  button('Cancel')?.click();
  flushSync();
  await Promise.resolve();
  expect(patched).not.toHaveBeenCalled();

  button('Change access')?.click();
  flushSync();
  chooseOperator();
  button('Save and sign them out')?.click();
  await eventually(() =>
    expect(patched).toHaveBeenCalledExactlyOnceWith(
      expect.objectContaining({ role: 'operator' })
    )
  );
  expect(button('Deactivate')?.disabled).toBe(true);
  expect(button('Reactivate')?.disabled).toBe(true);
  write.resolve(json({ ...member, role: 'operator', etag: 'saved' }));
  await eventually(() => expect(button('Deactivate')?.disabled).toBe(false));
  expect(button('Reactivate')?.disabled).toBe(false);
});
