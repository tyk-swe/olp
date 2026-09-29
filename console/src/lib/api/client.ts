import createClient from 'openapi-fetch';
import { parseManagementJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
import type { paths } from '$lib/api/schema';
import { serializeIfMatch } from '$lib/api/http';
import { createAuthMiddleware } from '$lib/features/access/session/authMiddleware';
import { authLifecycle } from '$lib/features/access/session/lifecycle';

/** Typed management API client generated from the OpenAPI contract. Feature
 * api modules resolve its calls with unwrap, unwrapPage or ensureOk. */
export const apiClient = createClient<paths>({
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
for (const method of methods) {
  const operation = apiClient[method] as TextOperation;
  Object.defineProperty(apiClient, method, {
    configurable: true,
    writable: true,
    value: async (path: string, options: Record<string, unknown> = {}) => {
      if (options.parseAs && options.parseAs !== 'json')
        return operation(path, options);
      const result = await operation(path, { ...options, parseAs: 'text' });
      // Non-2xx bodies arrive in the error arm as the same source text.
      // Decode problem documents so apiProblem keeps their type, detail, and
      // field errors; a body that is not JSON stays raw for that fallback.
      let error = result.error;
      if (typeof error === 'string' && error) {
        try {
          error = parseManagementJSON(error);
        } catch {
          // Keep the unstructured body.
        }
      }
      return {
        ...result,
        data: result.data ? parseManagementJSON(result.data) : undefined,
        error
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
