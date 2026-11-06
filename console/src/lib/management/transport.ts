import createClient, {
  type Client,
  type ClientPathsWithMethod,
  type FetchOptions,
  type MethodResponse,
  type Middleware
} from 'openapi-fetch';
import {
  parseManagementJSON,
  stringifyNativeJSON,
  type NativePath
} from '$lib/nativeJson';
import type { paths } from './schema';
import {
  requestOutcome,
  invalidResponse,
  type ApiOutcome,
  serializeIfMatch,
  unwrapOutcome,
  type Fetched
} from './http';

export function createManagementClient(options: {
  origin: string;
  fetch: (request: Request) => Promise<Response>;
  authentication: Middleware;
}) {
  const client = createClient<paths>({
    baseUrl: options.origin,
    cache: 'no-store',
    credentials: 'same-origin',
    redirect: 'error',
    bodySerializer: (body: unknown) =>
      body instanceof FormData ? body : stringifyNativeJSON(body),
    fetch: options.fetch
  });
  client.use(
    {
      onRequest({ request }) {
        request.headers.set('accept', 'application/json');
        const ifMatch = request.headers.get('if-match');
        if (ifMatch) request.headers.set('if-match', serializeIfMatch(ifMatch));
        return request;
      }
    },
    options.authentication
  );
  return client;
}

type NativeMethod = 'get' | 'put' | 'post' | 'patch';
type ManagementClient = Client<paths>;
type NativeRequest = <
  Method extends NativeMethod,
  Path extends ClientPathsWithMethod<ManagementClient, Method>
>(
  method: Method,
  path: Path,
  init: FetchOptions<paths[Path][Method]> & { parseAs: 'text' }
) => Promise<Fetched<string>>;

export async function requestNative<
  Method extends NativeMethod,
  Path extends ClientPathsWithMethod<ManagementClient, Method>
>(
  client: ManagementClient,
  method: Method,
  path: Path,
  init: FetchOptions<paths[Path][Method]>,
  nativePaths: readonly NativePath[]
): Promise<MethodResponse<ManagementClient, Method, Path>> {
  const request = client.request as NativeRequest;
  const outcome = await requestOutcome(() =>
    request(method, path, Object.assign({}, init, { parseAs: 'text' as const }))
  );
  const source = unwrapOutcome(outcome);
  return unwrapOutcome(nativeResponse(source, nativePaths)) as MethodResponse<
    ManagementClient,
    Method,
    Path
  >;
}

function nativeResponse(
  source: string,
  paths: readonly NativePath[]
): ApiOutcome<unknown> {
  try {
    return { kind: 'success', value: parseManagementJSON(source, paths) };
  } catch {
    return invalidResponse('The API response is not valid JSON');
  }
}
