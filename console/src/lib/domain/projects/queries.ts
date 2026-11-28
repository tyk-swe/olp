import { queryOptions } from '@tanstack/svelte-query';
import {
  listAllProjectMembers,
  listProjectMemberships,
  listProjects
} from './api.js';
import { projectKeys } from './projectKeys.js';

export const projectQueries = {
  all: () =>
    queryOptions({
      queryKey: projectKeys.all,
      queryFn: ({ signal }) => listProjects(signal)
    }),
  memberships: () =>
    queryOptions({
      queryKey: projectKeys.memberships,
      queryFn: ({ signal }) => listProjectMemberships(signal),
      staleTime: 0,
      gcTime: 0
    }),
  allMembers: (projectId: string) =>
    queryOptions({
      queryKey: projectKeys.allMembers(projectId),
      queryFn: ({ signal }) => listAllProjectMembers(projectId, signal)
    })
};
