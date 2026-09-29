import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type Project = Schemas['ProjectDetailResponse'];
export type ProjectMember = Schemas['ProjectMemberResponse'];
export type ProjectMembership = Schemas['ProjectMembershipItem'];
export type ProjectRole = ProjectMembership['role'];

export async function listProjectPage(
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

export async function listProjectMemberships(
  signal?: AbortSignal
): Promise<ProjectMembership[]> {
  return unwrap(
    await apiClient.GET('/api/v1/project-memberships', {
      signal
    })
  ).items;
}

export async function createProject(name: string): Promise<Project> {
  return unwrap(
    await apiClient.POST('/api/v1/projects', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: { name }
    })
  );
}

export async function renameProject(
  project: Project,
  name: string
): Promise<Project> {
  return unwrap(
    await apiClient.PATCH('/api/v1/projects/{project_id}', {
      params: {
        path: { project_id: project.id },
        header: { 'If-Match': project.etag }
      },
      body: { name }
    })
  );
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

export async function putProjectMember(
  project: Project,
  userId: string,
  role: ProjectRole
): Promise<ProjectMember> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/members/{user_id}', {
      params: {
        path: { project_id: project.id, user_id: userId },
        header: { 'If-Match': project.etag }
      },
      body: { role }
    })
  );
}

export async function removeProjectMember(
  project: Project,
  userId: string
): Promise<void> {
  ensureOk(
    await apiClient.DELETE('/api/v1/projects/{project_id}/members/{user_id}', {
      params: {
        path: { project_id: project.id, user_id: userId },
        header: { 'If-Match': project.etag }
      }
    })
  );
}
