/** Calls a same-origin inference gateway endpoint. The console session cookie
 * is never sent — the caller authenticates with an API key — and redirects are
 * refused rather than followed. */
export function gatewayFetch(
  path: string,
  init: RequestInit = {}
): Promise<Response> {
  return globalThis.fetch(path, {
    ...init,
    cache: 'no-store',
    credentials: 'omit',
    redirect: 'error'
  });
}
