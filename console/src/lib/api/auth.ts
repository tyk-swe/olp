import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem, ensureSuccess, result } from './http';
import { isFixedRole, type FixedRole } from '$lib/auth/authorization';

type Schemas = components['schemas'];

export type { FixedRole };
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
    typeof user?.id !== 'string' ||
    typeof user?.email !== 'string' ||
    typeof user?.display_name !== 'string' ||
    !isFixedRole(user?.role)
  ) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The session response is invalid',
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

export async function beginOidcLogin(returnTo: string, signal?: AbortSignal): Promise<string> {
  const { data, error, response } = await apiClient.POST('/api/v1/oidc/login', {
    body: { return_to: returnTo },
    signal
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
  const { data, error, response } = await apiClient.GET('/api/v1/sessions/current', { signal });
  return sessionResult(data, error, response);
}

export async function login(
  email: string,
  password: string,
  signal?: AbortSignal
): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.POST('/api/v1/sessions', {
    body: { email, password },
    signal
  });
  return sessionResult(data, error, response);
}

export async function acceptInvitation(
  input: Schemas['AcceptInvitationRequest'],
  signal?: AbortSignal
): Promise<CurrentSession> {
  const { data, error, response } = await apiClient.POST('/api/v1/invitations/accept', {
    body: input,
    signal
  });
  return sessionResult(data, error, response);
}

export async function logout(signal?: AbortSignal): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/sessions/current', { signal });
  // An absent server-side session is already the desired end state. The
  // lifecycle boundary has already hidden protected content and cleared its
  // local authority before this request is sent.
  if (response.status !== 401) ensureSuccess(error, response);
}
