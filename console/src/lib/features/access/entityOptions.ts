import { createQuery } from '@tanstack/svelte-query';
import { named, type EntityOption } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listBudgetGroups } from './budget-groups/api.js';
import { budgetGroupKeys } from './budget-groups/budgetGroupKeys.js';
import { listOrganizations } from './organizations/api.js';
import { organizationKeys } from './organizations/organizationKeys.js';
import { listProjectMemberships } from './projects/api.js';
import { projectKeys } from './projects/projectKeys.js';
import { listUsers, type User } from './users/api.js';
import { userKeys } from './users/userKeys.js';

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

export function budgetGroupOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal),
    enabled: allows('GET /api/v1/budget-groups'),
    select: (groups: { id: string; name: string }[]) => groups.map(named)
  }));
}
