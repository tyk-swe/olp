import { createQuery } from '@tanstack/svelte-query';
import type { EntityOption } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listUsers, type User } from './api.js';
import { userKeys } from './userKeys.js';

const person = (user: User): EntityOption => ({
  value: user.id,
  label: user.display_name || user.email,
  detail: user.active ? user.email : `${user.email}, deactivated`
});

export function personOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: userKeys.roster,
    queryFn: ({ signal }) => listUsers(signal),
    enabled: allows('GET /api/v1/users'),
    select: (users: User[]) => users.map(person)
  }));
}
