import type { components } from '../schema';
import { apiClient } from '../client';
import { ensureSuccess } from '../http';
import { result, type CursorPage } from './shared';

type Schemas = components['schemas'];

export type User = Schemas['UserDetailResponse'];
export type Invitation = Schemas['InvitationResponse'];
export type InvitationSecret = Schemas['CreateInvitationResponse'];
export type Session = Schemas['SessionDetailResponse'];

export async function listUserPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<User>> {
  const response = await apiClient.GET('/api/v1/users', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}

export async function updateUserRole(user: User, role: string): Promise<User> {
  const response = await apiClient.PATCH('/api/v1/users/{user_id}', {
    params: { path: { user_id: user.id }, header: { 'If-Match': user.etag } },
    body: { role }
  });
  return result(response.data, response.error, response.response);
}

export async function updateUserActive(user: User, active: boolean): Promise<User> {
  const response = await apiClient.PATCH('/api/v1/users/{user_id}', {
    params: { path: { user_id: user.id }, header: { 'If-Match': user.etag } },
    body: { active }
  });
  return result(response.data, response.error, response.response);
}

export async function listInvitationPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Invitation>> {
  const response = await apiClient.GET('/api/v1/invitations', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}

export async function createInvitation(email: string, role: string): Promise<InvitationSecret> {
  const response = await apiClient.POST('/api/v1/invitations', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: { email, role }
  });
  return result(response.data, response.error, response.response);
}

export async function revokeInvitation(id: string): Promise<void> {
  const response = await apiClient.DELETE('/api/v1/invitations/{invitation_id}', {
    params: { path: { invitation_id: id }, header: { 'Idempotency-Key': crypto.randomUUID() } }
  });
  result(response.data, response.error, response.response);
}

export async function listSessionPage(
  userId?: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Session>> {
  const response = await apiClient.GET('/api/v1/sessions', {
    params: { query: { limit: 50, user_id: userId, cursor } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}

export async function revokeSession(id: string): Promise<void> {
  const response = await apiClient.DELETE('/api/v1/sessions/{session_id}', {
    params: { path: { session_id: id } }
  });
  ensureSuccess(response.error, response.response);
}
