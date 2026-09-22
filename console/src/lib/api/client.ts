import createClient from 'openapi-fetch';
import { parseManagementJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
import type { paths } from '$lib/api/schema';
import { serializeIfMatch } from '$lib/api/http';
import { createAuthMiddleware } from '$lib/features/access/session/authMiddleware';
import { authLifecycle } from '$lib/features/access/session/lifecycle';

/** Generated-schema client for feature slices that need operation-level types. */
const generatedClient = createClient<paths>({
  // openapi-fetch constructs Request objects before invoking fetch. An
  // explicit same-origin base keeps those requests valid in browsers, tests,
  // and static-console integration without introducing a configurable API
  // origin.
  baseUrl: globalThis.location?.origin ?? 'http://127.0.0.1',
  cache: 'no-store',
  credentials: 'same-origin',
  redirect: 'error',
  bodySerializer: (body: unknown) =>
    body instanceof FormData ? body : stringifyNativeJSON(body),
  // Resolve fetch at call time so browser instrumentation and unit-test
  // transports observe the same generated request object.
  fetch: (request) => globalThis.fetch(request)
});

// Ask the generated transport for source text, then decode at this single
// boundary. Its default JSON path otherwise calls JSON.parse directly for
// chunked responses, bypassing Response.json overrides. Explicit streaming,
// binary and text callers retain their requested transport behavior.
const methods = [
  'GET',
  'PUT',
  'POST',
  'DELETE',
  'OPTIONS',
  'HEAD',
  'PATCH',
  'TRACE'
] as const;
type TextOperation = (
  path: string,
  options: Record<string, unknown>
) => Promise<{
  data?: string;
  error?: unknown;
  response: Response;
}>;
export const apiClient = generatedClient;
for (const method of methods) {
  const operation = generatedClient[method] as TextOperation;
  Object.defineProperty(apiClient, method, {
    configurable: true,
    writable: true,
    value: async (path: string, options: Record<string, unknown> = {}) => {
      if (options.parseAs && options.parseAs !== 'json')
        return operation(path, options);
      const result = await operation(path, { ...options, parseAs: 'text' });
      return result.data === undefined
        ? result
        : {
            ...result,
            data: result.data ? parseManagementJSON(result.data) : undefined
          };
    }
  });
}

apiClient.use({
  async onRequest({ request }) {
    request.headers.set('accept', 'application/json');
    const ifMatch = request.headers.get('if-match');
    if (ifMatch) request.headers.set('if-match', serializeIfMatch(ifMatch));
    return request;
  }
});
apiClient.use(createAuthMiddleware(authLifecycle));
