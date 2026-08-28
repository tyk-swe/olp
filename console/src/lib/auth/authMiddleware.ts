import type { Middleware } from 'openapi-fetch';
import type { AuthenticationLifecycle } from './lifecycle';

/**
 * The transport hook that ties API requests to the authentication lifecycle:
 * mutations wait for a fresh session and carry the CSRF token, every request
 * is bound to the current principal's abort signal, and a 401 (or a rotated
 * CSRF header) on the response feeds back into the lifecycle.
 */
export function createAuthMiddleware(
  lifecycle: AuthenticationLifecycle
): Middleware {
  return {
    async onRequest({ request }) {
      return lifecycle.prepareRequest(request);
    },
    async onResponse({ request, response }) {
      await lifecycle.handleResponse(request, response);
      return response;
    }
  };
}
