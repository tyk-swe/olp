import { expect, it, vi } from 'vitest';
import type { User } from './api';
import { updateMembers } from './bulk';

it('keeps each observed ETag and reports successful and stale members separately', async () => {
  const first = { id: 'first', etag: 'observed-first' } as User;
  const stale = { id: 'stale', etag: 'observed-stale' } as User;
  const final = { id: 'final', etag: 'observed-final' } as User;
  const update = vi.fn().mockImplementation(async (user: User) => {
    if (user === stale)
      throw new Error('The member changed; reload its current policy.');
    return { ...user, active: false, etag: 'updated' };
  });
  const result = await updateMembers(
    [first, stale, final],
    { active: false },
    update
  );
  expect(update.mock.calls.map(([user]) => user.etag)).toEqual([
    'observed-first',
    'observed-stale',
    'observed-final'
  ]);
  expect(result.updated.map((user) => user.id)).toEqual(['first', 'final']);
  expect(result.failed).toEqual([
    { user: stale, message: 'The member changed; reload its current policy.' }
  ]);
});
