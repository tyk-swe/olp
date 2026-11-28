import { unwrap, unwrapPage } from '#lib/management/http.js';
import type { CursorPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';

type Schemas = components['schemas'];

export type Project = Schemas['ProjectDetailResponse'];
export type ProjectMember = Schemas['ProjectMemberResponse'];
export type ProjectMembership = Schemas['ProjectMembershipItem'];

async function listProjectPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Project>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/projects', {
      params: { query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function listProjects(signal?: AbortSignal): Promise<Project[]> {
  return collectCursorPages((cursor) => listProjectPage(cursor, signal));
}

export async function listProjectMemberships(
  signal?: AbortSignal
): Promise<ProjectMembership[]> {
  return unwrap(
    await apiClient.GET('/api/v1/project-memberships', {
      signal
    })
  ).items;
}

export async function listProjectMemberPage(
  projectId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProjectMember>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/projects/{project_id}/members', {
      params: { path: { project_id: projectId }, query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function listAllProjectMembers(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectMember[]> {
  return collectCursorPages((cursor) =>
    listProjectMemberPage(projectId, cursor, signal)
  );
}
