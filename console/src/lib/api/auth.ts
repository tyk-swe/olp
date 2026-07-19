import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem, throwApiProblem } from './http';
import { clearCsrfToken, setCsrfToken } from './session';

export type FixedRole = 'owner' | 'operator' | 'developer' | 'viewer';

type Schemas = components['schemas'];

export type SessionUser = Schemas['UserResponse'] & { role: FixedRole };

export type CurrentSession = Omit<Schemas['SessionResponse'], 'user'> & { user: SessionUser };

function sessionResult(
  data: Schemas['SessionResponse'] | undefined,
  error: unknown,
  response: Response
): CurrentSession {
  if (!data) throwApiProblem(error, response);
  if (!['owner', 'operator', 'developer', 'viewer'].includes(data.user.role)) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The session response contains an invalid role',
      status: 502
    });
  }
  return data as CurrentSession;
}

export async function currentSession(signal?: AbortSignal): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.GET('/api/v1/sessions/current', { signal });
  const session = sessionResult(data, error, response);
  if (session.csrf_token) setCsrfToken(session.csrf_token);
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
  if (!response.ok && response.status !== 401) throwApiProblem(error, response);
  clearCsrfToken();
}
