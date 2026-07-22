import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem, ensureSuccess, result } from './http';
import { clearCsrfToken, getCsrfTokenVersion, setCsrfToken } from './session';

const FIXED_ROLE_VALUES = ['owner', 'operator', 'developer', 'viewer'] as const;

export type FixedRole = (typeof FIXED_ROLE_VALUES)[number];

const FIXED_ROLES = new Set<string>(FIXED_ROLE_VALUES);

type Schemas = components['schemas'];

export type SessionUser = Schemas['UserResponse'] & { role: FixedRole };

export type CurrentSession = Omit<Schemas['SessionResponse'], 'user'> & { user: SessionUser };

export type AuthenticationCapabilities = Schemas['AuthenticationCapabilities'];

function sessionResult(
  data: Schemas['SessionResponse'] | undefined,
  error: unknown,
  response: Response
): CurrentSession {
  const value = result(data, error, response);
  const user = value.user as Partial<Schemas['UserResponse']> | null | undefined;
  if (
    typeof value.csrf_token !== 'string' ||
    value.csrf_token.length === 0 ||
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

export async function authenticationCapabilities(
  signal?: AbortSignal
): Promise<AuthenticationCapabilities> {
  const { data, error, response } = await apiClient.GET('/api/v1/auth/capabilities', { signal });
  const value = result(data, error, response);
  if (
    typeof value.local_login_enabled !== 'boolean' ||
    typeof value.oidc_login_enabled !== 'boolean'
  ) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The authentication capabilities response is invalid',
      status: 502
    });
  }
  return value;
}

export async function beginOidcLogin(returnTo: string): Promise<string> {
  const { data, error, response } = await apiClient.POST('/api/v1/oidc/login', {
    body: { return_to: returnTo }
  });
  const value = result(data, error, response);
  if (typeof value.authorization_url !== 'string') {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The OIDC authorization response is invalid',
      status: 502
    });
  }
  try {
    const authorizationUrl = new URL(value.authorization_url);
    if (!['https:', 'http:'].includes(authorizationUrl.protocol)) throw new Error('invalid scheme');
    return authorizationUrl.href;
  } catch {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The OIDC authorization response is invalid',
      status: 502
    });
  }
}

export async function currentSession(signal?: AbortSignal): Promise<CurrentSession> {
  // A current-session recovery can race a login or security transition. If a
  // newer response has already installed CSRF state, do not let this older
  // session response replace it after its body finishes parsing.
  const csrfTokenVersion = getCsrfTokenVersion();
  const { data, error, response } = await apiClient.GET('/api/v1/sessions/current', { signal });
  const session = sessionResult(data, error, response);
  if (getCsrfTokenVersion() === csrfTokenVersion) setCsrfToken(session.csrf_token);
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
