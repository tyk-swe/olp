import { vi } from 'vitest';

type RequestHandler = (request: Request, index: number) => Response | Promise<Response>;

export function captureRequests(handler: RequestHandler): Request[] {
  const requests: Request[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      requests.push(request);
      return handler(request, requests.length - 1);
    })
  );
  return requests;
}

export function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  if (!headers.has('content-type')) headers.set('content-type', 'application/json');
  return new Response(JSON.stringify(body), { ...init, headers });
}
