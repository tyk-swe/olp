import { createManagementClient } from './transport';
import { createAuthMiddleware } from '#lib/session/authMiddleware.js';
import { authLifecycle } from '#lib/session/lifecycle.js';

export const apiClient = createManagementClient({
  origin: globalThis.location?.origin ?? 'http://127.0.0.1',
  fetch: (request) => globalThis.fetch(request),
  authentication: createAuthMiddleware(authLifecycle)
});
