import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import type { ProjectMembership } from './api.js';
import { projectQueries } from './queries.js';

export function projectOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    ...projectQueries.memberships(),
    enabled: allows('GET /api/v1/project-memberships'),
    select: (projects: ProjectMembership[]) => projects.map(named)
  }));
}
