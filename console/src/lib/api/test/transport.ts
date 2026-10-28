import { expect, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';

export function serve(
  handler: (request: Request) => Response | Promise<Response>
) {
  authLifecycle.establishSession({
    user: {
      id: 'owner',
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: []
    },
    csrf_token: 'fixture-csrf'
  });
  vi.stubGlobal('fetch', handler);
}

export function json<T>(body: T, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' }
  });
}

export function route(request: Request, method: string, path: string) {
  expect(request.method).toBe(method);
  expect(new URL(request.url).pathname).toBe(path);
  expect(request.headers.get('Accept')).toBe('application/json');
  if (method !== 'GET')
    expect(request.headers.get('X-CSRF-Token')).toBe('fixture-csrf');
}
