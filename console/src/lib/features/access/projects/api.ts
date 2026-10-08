import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

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

/** Every project, following cursors past the first page. */
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

/** Every member of a project, following cursors past the first page. */
export async function listAllProjectMembers(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectMember[]> {
  return collectCursorPages((cursor) =>
    listProjectMemberPage(projectId, cursor, signal)
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

export type ProjectEndUserPolicy = Schemas['ProjectEndUserPolicy'];
export async function getProjectEndUserPolicy(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectEndUserPolicy> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/end-user-policy', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}
export async function putProjectEndUserPolicy(
  projectId: string,
  etag: string,
  policy: Schemas['EndUserPolicy'] | null
): Promise<ProjectEndUserPolicy> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/end-user-policy', {
      params: { path: { project_id: projectId }, header: { 'If-Match': etag } },
      body: { policy }
    })
  );
}

export type ProjectAttributionPolicy = Schemas['ProjectAttributionPolicy'];
export async function getProjectAttributionPolicy(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectAttributionPolicy> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/attribution-policy', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}
export async function putProjectAttributionPolicy(
  projectId: string,
  etag: string,
  policy: Schemas['AttributionPolicy'] | null
): Promise<ProjectAttributionPolicy> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/attribution-policy', {
      params: { path: { project_id: projectId }, header: { 'If-Match': etag } },
      body: { policy }
    })
  );
}

export type ProjectRouteGroups = Schemas['ProjectRouteGroups'];
export async function getProjectRouteGroups(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectRouteGroups> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/route-groups', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}
export async function putProjectRouteGroups(
  projectId: string,
  etag: string,
  groups: Schemas['RouteGroups']
): Promise<ProjectRouteGroups> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/route-groups', {
      params: { path: { project_id: projectId }, header: { 'If-Match': etag } },
      body: { groups }
    })
  );
}

export type ProjectLimitTemplates = Schemas['ProjectLimitTemplates'];
export async function getProjectLimitTemplates(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectLimitTemplates> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/limit-templates', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}
export async function putProjectLimitTemplates(
  projectId: string,
  etag: string,
  templates: Schemas['LimitTemplates']
): Promise<ProjectLimitTemplates> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/limit-templates', {
      params: { path: { project_id: projectId }, header: { 'If-Match': etag } },
      body: { templates }
    })
  );
}

export type ProjectAttributionBudgets = Schemas['ProjectAttributionBudgets'];
export async function getProjectAttributionBudgets(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectAttributionBudgets> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/attribution-budgets', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}
export async function putProjectAttributionBudgets(
  projectId: string,
  etag: string,
  budgets: Schemas['AttributionBudgets']
): Promise<ProjectAttributionBudgets> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/attribution-budgets', {
      params: { path: { project_id: projectId }, header: { 'If-Match': etag } },
      body: { budgets }
    })
  );
}
