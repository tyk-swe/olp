import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  createHeaders,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import type {
  Project,
  ProjectMember,
  ProjectMembership
} from '#lib/domain/projects/api.js';

type Schemas = components['schemas'];

export type ProjectRole = ProjectMembership['role'];

export async function getProject(
  projectId: string,
  signal?: AbortSignal
): Promise<Project> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}

export async function createProject(
  name: string,
  requestId: string
): Promise<Project> {
  return unwrap(
    await apiClient.POST('/api/v1/projects', {
      params: { header: createHeaders(requestId) },
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
        header: ifMatch(project.etag)
      },
      body: { name }
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
        header: ifMatch(project.etag)
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
        header: ifMatch(project.etag)
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
      params: {
        path: { project_id: projectId },
        header: ifMatch(etag)
      },
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
      params: {
        path: { project_id: projectId },
        header: ifMatch(etag)
      },
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
      params: {
        path: { project_id: projectId },
        header: ifMatch(etag)
      },
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
      params: {
        path: { project_id: projectId },
        header: ifMatch(etag)
      },
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
      params: {
        path: { project_id: projectId },
        header: ifMatch(etag)
      },
      body: { budgets }
    })
  );
}

export type Publication = Schemas['CatalogPublicationResponse'];
export async function getPublication(
  id: string,
  signal?: AbortSignal
): Promise<Publication> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/catalog', {
      params: { path: { project_id: id } },
      signal
    })
  );
}
export async function putPublication(
  id: string,
  value: Publication
): Promise<Publication> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/catalog', {
      params: { path: { project_id: id }, header: ifMatch(value.etag) },
      body: { enabled: value.enabled, prices_public: value.prices_public }
    })
  );
}

export const deleteProject = async (
  value: { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/projects/{project_id}', {
      params: {
        path: { project_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
