import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem, ensureSuccess, result } from './http';
import { clearCsrfToken, setCsrfToken } from './session';

const FIXED_ROLE_VALUES = ['owner', 'operator', 'developer', 'viewer'] as const;

export type FixedRole = (typeof FIXED_ROLE_VALUES)[number];

const FIXED_ROLES = new Set<string>(FIXED_ROLE_VALUES);

type Schemas = components['schemas'];

export type SessionUser = Schemas['UserResponse'] & { role: FixedRole };

export type CurrentSession = Omit<Schemas['SessionResponse'], 'user'> & { user: SessionUser };

function sessionResult(
  data: Schemas['SessionResponse'] | undefined,
  error: unknown,
  response: Response
): CurrentSession {
  const value = result(data, error, response);
  const user = value.user as Partial<Schemas['UserResponse']> | null | undefined;
  if (
    typeof value.csrf_token !== 'string' ||
    typeof user?.id !== 'string' ||
    typeof user?.email !== 'string' ||
    typeof user?.display_name !== 'string' ||
    typeof user?.role !== 'string'
  ) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The session response is invalid',
      status: 502
    });
  }
  if (!FIXED_ROLES.has(user.role)) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The session response contains an invalid role',
      status: 502
    });
  }
  return value as CurrentSession;
}

export async function currentSession(signal?: AbortSignal): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.GET('/api/v1/sessions/current', { signal });
  const session = sessionResult(data, error, response);
  if (session.csrf_token) {
    setCsrfToken(session.csrf_token);
  } else {
    clearCsrfToken();
  }
  return session;
}

export async function login(email: string, password: string): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.POST('/api/v1/sessions', {
    body: { email, password }
  });
  const session = sessionResult(data, error, response);
  setCsrfToken(session.csrf_token);
  return session;
}

export async function acceptInvitation(
  input: Schemas['AcceptInvitationRequest']
): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.POST('/api/v1/invitations/accept', {
    body: input
  });
  const session = sessionResult(data, error, response);
  setCsrfToken(session.csrf_token);
  return session;
}

export async function logout(): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/sessions/current');
  // An absent server-side session is already the desired end state. Treat it
  // as a completed logout so an expired session cannot trap the user on a
  // protected page with stale client-side CSRF state.
  if (response.status !== 401) ensureSuccess(error, response);
  clearCsrfToken();
}
