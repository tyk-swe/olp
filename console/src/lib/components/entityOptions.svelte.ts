import { createQuery } from '@tanstack/svelte-query';
import { useRole } from '#lib/features/access/session/useRole.svelte.js';
import { listUsers, type User } from '#lib/features/access/users/api.js';
import { userKeys } from '#lib/features/access/users/userKeys.js';
import {
  listProjectMemberships,
  type ProjectMembership
} from '#lib/features/access/projects/api.js';
import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
import { listProviders } from '#lib/features/providers/providers.js';
import { providerKeys } from '#lib/features/providers/providerKeys.js';
import type { ProviderSummary } from '#lib/features/providers/providers.js';
import {
  listOrganizations,
  type Organization
} from '#lib/features/access/organizations/api.js';
import { organizationKeys } from '#lib/features/access/organizations/organizationKeys.js';
import {
  listBudgetGroups,
  type BudgetGroup
} from '#lib/features/access/budget-groups/api.js';
import { budgetGroupKeys } from '#lib/features/access/budget-groups/budgetGroupKeys.js';

export type EntityKind =
  'person' | 'project' | 'provider' | 'organization' | 'budget-group';
export type EntityOption = { value: string; label: string; detail?: string };

const personOption = (user: User): EntityOption => ({
  value: user.id,
  label: user.display_name || user.email,
  detail: user.active ? user.email : `${user.email}, deactivated`
});

const named = (entity: { id: string; name: string }): EntityOption => ({
  value: entity.id,
  label: entity.name
});

export function entityOptions(kind: EntityKind) {
  const { allows } = useRole();
  switch (kind) {
    case 'person':
      return createQuery(() => ({
        queryKey: userKeys.roster,
        queryFn: ({ signal }) => listUsers(signal),
        enabled: allows('GET /api/v1/users'),
        select: (users: User[]) => users.map(personOption)
      }));
    case 'project':
      return createQuery(() => ({
        queryKey: projectKeys.memberships,
        queryFn: ({ signal }) => listProjectMemberships(signal),
        enabled: allows('GET /api/v1/project-memberships'),
        select: (projects: ProjectMembership[]) => projects.map(named)
      }));
    case 'provider':
      return createQuery(() => ({
        queryKey: providerKeys.all(),
        queryFn: ({ signal }) => listProviders(signal),
        enabled: allows('GET /api/v1/providers'),
        select: (providers: ProviderSummary[]) => providers.map(named)
      }));
    case 'organization':
      return createQuery(() => ({
        queryKey: organizationKeys.all,
        queryFn: ({ signal }) => listOrganizations(signal),
        enabled: allows('GET /api/v1/organizations'),
        select: (organizations: Organization[]) => organizations.map(named)
      }));
    case 'budget-group':
      return createQuery(() => ({
        queryKey: budgetGroupKeys.list(),
        queryFn: ({ signal }) => listBudgetGroups(signal),
        enabled: allows('GET /api/v1/budget-groups'),
        select: (groups: BudgetGroup[]) => groups.map(named)
      }));
  }
}
