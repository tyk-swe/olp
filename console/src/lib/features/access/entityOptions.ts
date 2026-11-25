import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listOrganizations } from './organizations/api.js';
import { organizationKeys } from './organizations/organizationKeys.js';
import { listProjectMemberships } from './projects/api.js';
import { projectKeys } from './projects/projectKeys.js';

export function projectOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal),
    enabled: allows('GET /api/v1/project-memberships'),
    select: (projects: { id: string; name: string }[]) => projects.map(named)
  }));
}

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
