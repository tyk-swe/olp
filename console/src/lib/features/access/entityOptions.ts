import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listOrganizations } from './organizations/api.js';
import { organizationKeys } from './organizations/organizationKeys.js';

export function organizationOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: organizationKeys.all,
    queryFn: ({ signal }) => listOrganizations(signal),
    enabled: allows('GET /api/v1/organizations'),
    select: (organizations: { id: string; name: string }[]) =>
      organizations.map(named)
  }));
}
