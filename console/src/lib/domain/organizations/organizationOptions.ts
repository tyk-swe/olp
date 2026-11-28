import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import type { Organization } from './api.js';
import { organizationQueries } from './queries.js';

export function organizationOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    ...organizationQueries.all(),
    enabled: allows('GET /api/v1/organizations'),
    select: (organizations: Organization[]) => organizations.map(named)
  }));
}
