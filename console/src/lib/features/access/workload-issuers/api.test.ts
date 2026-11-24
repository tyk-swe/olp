import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { listIssuers } from './api.js';

describe('listIssuers', () => {
  it('rejects when the workload issuers list repeats a cursor', async () => {
    const handler = vi.fn((request: Request) => {
      route(request, 'GET', '/api/v1/workload-issuers');
      return json({
        items: [],
        next_cursor: handler.mock.calls.length <= 2 ? 'same-page' : null
      });
    });
    serve(handler, ['read']);

    await expect(listIssuers()).rejects.toMatchObject({
      problem: { type: 'urn:olp:problem:invalid-cursor-cycle' }
    });
  });
});
