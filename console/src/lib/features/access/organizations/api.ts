import { apiClient } from '$lib/management/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/management/http';
import { collectCursorPages } from '$lib/management/pagination';
import type { components } from '$lib/management/schema';
export type Organization = components['schemas']['Organization'];
export async function listOrganizations(signal?: AbortSignal) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/organizations', {
        params: { query: { cursor } },
        signal
      })
    )
  );
}
export async function createOrganization(name: string, requestId: string) {
  return unwrap(
    await apiClient.POST('/api/v1/organizations', {
      params: { header: { 'Idempotency-Key': requestId } },
      body: { name }
    })
  );
}
export async function renameOrganization(
  organization_id: string,
  name: string,
  etag: string
) {
  return unwrap(
    await apiClient.PATCH('/api/v1/organizations/{organization_id}', {
      params: {
        path: { organization_id },
        header: { 'If-Match': `"${etag}"` }
      },
      body: { name }
    })
  );
}
export async function organizationMembers(
  organization_id: string,
  signal?: AbortSignal
) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/organizations/{organization_id}/members', {
        params: { path: { organization_id }, query: { cursor } },
        signal
      })
    )
  );
}
export async function putOrganizationMember(
  organization_id: string,
  user_id: string,
  role: 'manager' | 'viewer',
  etag: string
) {
  ensureOk(
    await apiClient.PUT(
      '/api/v1/organizations/{organization_id}/members/{user_id}',
      {
        params: {
          path: { organization_id, user_id },
          header: { 'If-Match': `"${etag}"` }
        },
        body: { role }
      }
    )
  );
}
export async function removeOrganizationMember(
  organization_id: string,
  user_id: string,
  etag: string
) {
  ensureOk(
    await apiClient.DELETE(
      '/api/v1/organizations/{organization_id}/members/{user_id}',
      {
        params: {
          path: { organization_id, user_id },
          header: { 'If-Match': `"${etag}"` }
        }
      }
    )
  );
}
export async function organizationProjects(
  organization_id: string,
  signal?: AbortSignal
) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/organizations/{organization_id}/projects', {
        params: { path: { organization_id }, query: { cursor } },
        signal
      })
    )
  );
}
export async function createOrganizationProject(
  organization_id: string,
  name: string,
  requestId: string
) {
  return unwrap(
    await apiClient.POST('/api/v1/organizations/{organization_id}/projects', {
      params: {
        path: { organization_id },
        header: { 'Idempotency-Key': requestId }
      },
      body: { name }
    })
  );
}
export async function projectMembers(
  organization_id: string,
  project_id: string,
  signal?: AbortSignal
) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET(
        '/api/v1/organizations/{organization_id}/projects/{project_id}/members',
        {
          params: { path: { organization_id, project_id }, query: { cursor } },
          signal
        }
      )
    )
  );
}
export async function putProjectMember(
  organization_id: string,
  project_id: string,
  user_id: string,
  role: 'manager' | 'viewer',
  etag: string
) {
  return unwrap(
    await apiClient.PUT(
      '/api/v1/organizations/{organization_id}/projects/{project_id}/members/{user_id}',
      {
        params: {
          path: { organization_id, project_id, user_id },
          header: { 'If-Match': `"${etag}"` }
        },
        body: { role }
      }
    )
  );
}
export async function removeProjectMember(
  organization_id: string,
  project_id: string,
  user_id: string,
  etag: string
) {
  ensureOk(
    await apiClient.DELETE(
      '/api/v1/organizations/{organization_id}/projects/{project_id}/members/{user_id}',
      {
        params: {
          path: { organization_id, project_id, user_id },
          header: { 'If-Match': `"${etag}"` }
        }
      }
    )
  );
}

export async function renameProject(
  organization_id: string,
  project_id: string,
  name: string,
  etag: string
) {
  return unwrap(
    await apiClient.PATCH(
      '/api/v1/organizations/{organization_id}/projects/{project_id}',
      {
        params: {
          path: { organization_id, project_id },
          header: { 'If-Match': `"${etag}"` }
        },
        body: { name }
      }
    )
  );
}
