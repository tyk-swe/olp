import { createManagementClient } from './transport';
import { createAuthMiddleware } from '$lib/features/access/session/authMiddleware';
import { authLifecycle } from '$lib/features/access/session/lifecycle';

export const apiClient = createManagementClient({
  origin: globalThis.location?.origin ?? 'http://127.0.0.1',
  fetch: (request) => globalThis.fetch(request),
  authentication: createAuthMiddleware(authLifecycle)
});
