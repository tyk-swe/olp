import { confirmMFA } from '$lib/features/access/mfa/prompt.svelte';
import type { components } from '$lib/api/schema';
import { withAuthenticationDeadline } from './requestDeadline';
import { apiClient } from '$lib/api/client';
import { ApiProblem, ensureOk, unwrap } from '$lib/api/http';
import {
  isFixedRole,
  type FixedRole
} from '$lib/features/access/session/authorization';

type Schemas = components['schemas'];

export type SessionUser = Schemas['UserResponse'] & {
  role: FixedRole;
  operations: readonly Schemas['ManagementOperation'][];
};
export type CurrentSession = Omit<Schemas['SessionResponse'], 'user'> & {
  user: SessionUser;
};

export type AuthenticationCapabilities = Schemas['AuthenticationCapabilities'];

function sessionResult(fetched: {
  data?: Schemas['SessionResponse'];
  error?: unknown;
  response: Response;
}): CurrentSession {
  const value = unwrap(fetched);
  const user = value.user as
    Partial<Schemas['UserResponse']> | null | undefined;
  if (
    typeof value.csrf_token !== 'string' ||
    typeof user?.id !== 'string' ||
    typeof user?.email !== 'string' ||
    typeof user?.display_name !== 'string' ||
    !isFixedRole(user?.role) ||
    (user?.access_scope !== 'global' && user?.access_scope !== 'assigned') ||
    !Array.isArray(value.operations) ||
    !value.operations.every((operation) => typeof operation === 'string')
  ) {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The session response is invalid',
      status: 502
    });
  }
  // The console authorizes against the member's operations, which the
  // session reports beside the member.
  return {
    ...value,
    user: { ...(value.user as SessionUser), operations: value.operations }
  };
}

export async function authenticationCapabilities(
  signal?: AbortSignal
): Promise<AuthenticationCapabilities> {
  const value = unwrap(
    await withAuthenticationDeadline(
      (signal) => apiClient.GET('/api/v1/auth/capabilities', { signal }),
      signal
    )
  );
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

export async function beginOidcLogin(
  returnTo: string,
  signal?: AbortSignal
): Promise<string> {
  const value = unwrap(
    await apiClient.POST('/api/v1/oidc/login', {
      body: { return_to: returnTo },
      signal
    })
  );
  if (typeof value.authorization_url !== 'string') {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The OIDC authorization response is invalid',
      status: 502
    });
  }
  try {
    const authorizationUrl = new URL(value.authorization_url);
    if (!['https:', 'http:'].includes(authorizationUrl.protocol))
      throw new Error('invalid scheme');
    return authorizationUrl.href;
  } catch {
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The OIDC authorization response is invalid',
      status: 502
    });
  }
}

export async function currentSession(
  signal?: AbortSignal
): Promise<CurrentSession> {
  return sessionResult(
    await withAuthenticationDeadline(
      (signal) => apiClient.GET('/api/v1/sessions/current', { signal }),
      signal
    )
  );
}

export async function login(
  email: string,
  password: string,
  signal?: AbortSignal
): Promise<CurrentSession> {
  const fetched = await apiClient.POST('/api/v1/sessions', {
    body: { email, password },
    signal
  });
  return completeAuthentication(fetched, signal);
}

export async function acceptInvitation(
  input: Schemas['AcceptInvitationRequest'],
  signal?: AbortSignal
): Promise<CurrentSession> {
  const fetched = await apiClient.POST('/api/v1/invitations/accept', {
    body: input,
    signal
  });
  return completeAuthentication(fetched, signal);
}

export async function logout(signal?: AbortSignal): Promise<void> {
  const fetched = await apiClient.DELETE('/api/v1/sessions/current', {
    signal
  });
  // An absent server-side session is already the desired end state. The
  // lifecycle boundary has already hidden protected content and cleared its
  // local authority before this request is sent.
  if (fetched.response.status !== 401) ensureOk(fetched);
}

async function completeAuthentication(
  fetched: {
    data?: Schemas['SessionResponse'] | Schemas['MFAChallenge'];
    error?: unknown;
    response: Response;
  },
  signal?: AbortSignal
): Promise<CurrentSession> {
  if (fetched.response.status === 202) {
    const data = unwrap(fetched);
    if (!('challenge' in data))
      throw new Error('Invalid authentication challenge.');
    await confirmMFA({ challenge: data }, signal);
    return currentSession(signal);
  }
  return sessionResult({
    ...fetched,
    data: fetched.data as Schemas['SessionResponse'] | undefined
  });
}
