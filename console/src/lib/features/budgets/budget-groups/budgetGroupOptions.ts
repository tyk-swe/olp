import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { budgetGroupQueries } from '#lib/domain/budget-groups/queries.js';

export function budgetGroupOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    ...budgetGroupQueries.all(),
    enabled: allows('GET /api/v1/budget-groups'),
    select: (groups: { id: string; name: string }[]) => groups.map(named)
  }));
}
