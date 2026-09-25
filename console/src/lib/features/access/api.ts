import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureSuccess, pageResult, result } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type User = Schemas['UserDetailResponse'];
export type Invitation = Schemas['InvitationResponse'];
export type InvitationSecret = Schemas['CreateInvitationResponse'];
export type Session = Schemas['SessionDetailResponse'];
export type UserPatch = Schemas['UpdateUserRoleRequest'];

export async function listUserPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<User>> {
  const response = await apiClient.GET('/api/v3/users', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function listUsers(signal?: AbortSignal): Promise<User[]> {
  return collectCursorPages((cursor) => listUserPage(cursor, signal));
}

export async function updateUser(user: User, patch: UserPatch): Promise<User> {
  const response = await apiClient.PATCH('/api/v3/users/{user_id}', {
    params: { path: { user_id: user.id }, header: { 'If-Match': user.etag } },
    body: patch
  });
  return result(response.data, response.error, response.response);
}

export async function listInvitationPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Invitation>> {
  const response = await apiClient.GET('/api/v3/invitations', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function createInvitation(
  email: string,
  role: string,
  expiresInHours?: number
): Promise<InvitationSecret> {
  const response = await apiClient.POST('/api/v3/invitations', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    // Omitting the field lets the API apply its seven-day default; it caps the
    // lifetime at thirty days (720 hours).
    body: {
      email,
      role,
      ...(expiresInHours ? { expires_in_hours: expiresInHours } : {})
    }
  });
  return result(response.data, response.error, response.response);
}

export async function revokeInvitation(id: string): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v3/invitations/{invitation_id}',
    {
      params: {
        path: { invitation_id: id },
        header: { 'Idempotency-Key': crypto.randomUUID() }
      }
    }
  );
  result(response.data, response.error, response.response);
}

export async function listSessionPage(
  userId?: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Session>> {
  const response = await apiClient.GET('/api/v3/sessions', {
    params: { query: { limit: 50, user_id: userId, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function revokeSession(id: string): Promise<void> {
  const response = await apiClient.DELETE('/api/v3/sessions/{session_id}', {
    params: { path: { session_id: id } }
  });
  ensureSuccess(response.error, response.response);
}

export type ManagementToken = Schemas['ManagementTokenResponse'];
export type ManagementTokenSecret = Schemas['CreateManagementTokenResponse'];

export const MANAGEMENT_TOKEN_SCOPES = [
  'read',
  'access_read',
  'access',
  'settings',
  'configure',
  'keys',
  'playground',
  'usage'
] as const;

export type ManagementTokenScope = (typeof MANAGEMENT_TOKEN_SCOPES)[number];

export async function listManagementTokenPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ManagementToken>> {
  const response = await apiClient.GET('/api/v3/management-tokens', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function createManagementToken(
  name: string,
  scopes: ManagementTokenScope[],
  expiresAt: string,
  projectIds?: string[]
): Promise<ManagementTokenSecret> {
  const response = await apiClient.POST('/api/v3/management-tokens', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: {
      name,
      scopes,
      expires_at: expiresAt,
      ...(projectIds ? { project_ids: projectIds } : {})
    }
  });
  return result(response.data, response.error, response.response);
}

export async function revokeManagementToken(
  id: string,
  etag: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v3/management-tokens/{management_token_id}/revoke',
    {
      params: {
        path: { management_token_id: id },
        header: {
          'If-Match': etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  ensureSuccess(response.error, response.response);
}

export type Project = Schemas['ProjectDetailResponse'];
export type ProjectMember = Schemas['ProjectMemberResponse'];
export type ProjectMembership = Schemas['ProjectMembershipItem'];
export type ProjectRole = ProjectMembership['role'];

export async function listProjectPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Project>> {
  const response = await apiClient.GET('/api/v3/projects', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function listProjects(signal?: AbortSignal): Promise<Project[]> {
  return collectCursorPages((cursor) => listProjectPage(cursor, signal));
}

export async function listProjectMemberships(
  signal?: AbortSignal
): Promise<ProjectMembership[]> {
  const response = await apiClient.GET('/api/v3/project-memberships', {
    signal
  });
  return result(response.data, response.error, response.response).items;
}

export async function createProject(name: string): Promise<Project> {
  const response = await apiClient.POST('/api/v3/projects', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: { name }
  });
  return result(response.data, response.error, response.response);
}

export async function renameProject(
  project: Project,
  name: string
): Promise<Project> {
  const response = await apiClient.PATCH('/api/v3/projects/{project_id}', {
    params: {
      path: { project_id: project.id },
      header: { 'If-Match': project.etag }
    },
    body: { name }
  });
  return result(response.data, response.error, response.response);
}

export async function listProjectMemberPage(
  projectId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProjectMember>> {
  const response = await apiClient.GET(
    '/api/v3/projects/{project_id}/members',
    {
      params: { path: { project_id: projectId }, query: { limit: 50, cursor } },
      signal
    }
  );
  return pageResult(result(response.data, response.error, response.response));
}

export async function putProjectMember(
  project: Project,
  userId: string,
  role: ProjectRole
): Promise<ProjectMember> {
  const response = await apiClient.PUT(
    '/api/v3/projects/{project_id}/members/{user_id}',
    {
      params: {
        path: { project_id: project.id, user_id: userId },
        header: { 'If-Match': project.etag }
      },
      body: { role }
    }
  );
  return result(response.data, response.error, response.response);
}

export async function removeProjectMember(
  project: Project,
  userId: string
): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v3/projects/{project_id}/members/{user_id}',
    {
      params: {
        path: { project_id: project.id, user_id: userId },
        header: { 'If-Match': project.etag }
      }
    }
  );
  ensureSuccess(response.error, response.response);
}
