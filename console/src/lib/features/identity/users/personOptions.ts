import { createQuery } from '@tanstack/svelte-query';
import type { EntityOption } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { type User } from '#lib/domain/users/api.js';
import { userQueries } from '#lib/domain/users/queries.js';

const person = (user: User): EntityOption => ({
  value: user.id,
  label: user.display_name || user.email,
  detail: user.active ? user.email : `${user.email}, deactivated`
});

export function personOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    ...userQueries.all(),
    enabled: allows('GET /api/v1/users'),
    select: (users: User[]) => users.map(person)
  }));
}
