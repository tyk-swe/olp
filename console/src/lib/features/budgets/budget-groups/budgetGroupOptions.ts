import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listBudgetGroups } from './api.js';
import { budgetGroupKeys } from './budgetGroupKeys.js';

export function budgetGroupOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal),
    enabled: allows('GET /api/v1/budget-groups'),
    select: (groups: { id: string; name: string }[]) => groups.map(named)
  }));
}
