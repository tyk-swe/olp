import createClient from 'openapi-fetch';
import type { paths } from './schema';
import { serializeIfMatch } from './http';
import { getCsrfToken, setCsrfToken } from './session';

/** Generated-schema client for feature slices that need operation-level types. */
export const apiClient = createClient<paths>({
  // openapi-fetch constructs Request objects before invoking fetch. An
  // explicit same-origin base keeps those requests valid in browsers, tests,
  // and static-console integration without introducing a configurable API
  // origin.
  baseUrl: globalThis.location?.origin ?? 'http://127.0.0.1',
  cache: 'no-store',
  credentials: 'same-origin',
  redirect: 'error',
  // Resolve fetch at call time so browser instrumentation and unit-test
  // transports observe the same generated request object.
  fetch: (request) => globalThis.fetch(request)
});

apiClient.use({
  async onRequest({ request }) {
    request.headers.set('accept', 'application/json');
    const ifMatch = request.headers.get('if-match');
    if (ifMatch) request.headers.set('if-match', serializeIfMatch(ifMatch));
    if (!['GET', 'HEAD', 'OPTIONS'].includes(request.method)) {
      const csrf = getCsrfToken();
      if (csrf) request.headers.set('x-csrf-token', csrf);
    }
    return request;
  },
  async onResponse({ response }) {
    const rotatedCsrf = response.headers.get('x-csrf-token');
    if (rotatedCsrf) setCsrfToken(rotatedCsrf);
    return response;
  }
});
