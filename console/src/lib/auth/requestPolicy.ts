import type { paths } from '$lib/api/schema';

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

type AuthenticationPath = Extract<
  keyof paths,
  | '/api/v1/setup/status'
  | '/api/v1/setup'
  | '/api/v1/sessions'
  | '/api/v1/invitations/accept'
  | '/api/v1/auth/capabilities'
  | '/api/v1/oidc/login'
  | '/api/v1/oidc/callback'
>;

type AuthenticationRoute = Readonly<{
  method: 'GET' | 'POST';
  path: AuthenticationPath;
}>;

const AUTHENTICATION_ROUTES = [
  { method: 'GET', path: '/api/v1/setup/status' },
  { method: 'POST', path: '/api/v1/setup' },
  { method: 'POST', path: '/api/v1/sessions' },
  { method: 'POST', path: '/api/v1/invitations/accept' },
  { method: 'GET', path: '/api/v1/auth/capabilities' },
  { method: 'GET', path: '/api/v1/oidc/login' },
  { method: 'POST', path: '/api/v1/oidc/login' },
  { method: 'GET', path: '/api/v1/oidc/callback' }
] as const satisfies readonly AuthenticationRoute[];

function endpoint(request: Request): { method: string; pathname: string } {
  const url = new URL(request.url);
  return { method: request.method.toUpperCase(), pathname: url.pathname };
}

export function isAuthenticationEndpoint(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return AUTHENTICATION_ROUTES.some(
    (route) => route.method === method && route.path === pathname
  );
}

export function isSessionValidationEndpoint(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return method === 'GET' && pathname === '/api/v1/sessions/current';
}

export function isCurrentSessionDeletion(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return method === 'DELETE' && pathname === '/api/v1/sessions/current';
}

export function isMutationRequest(request: Request): boolean {
  return !SAFE_METHODS.has(request.method.toUpperCase());
}

export function combineSignals(...signals: AbortSignal[]): AbortSignal {
  if (typeof AbortSignal.any === 'function') return AbortSignal.any(signals);
  const controller = new AbortController();
  for (const signal of signals) {
    if (signal.aborted) {
      controller.abort(signal.reason);
      break;
    }
    signal.addEventListener('abort', () => controller.abort(signal.reason), {
      once: true
    });
  }
  return controller.signal;
}
