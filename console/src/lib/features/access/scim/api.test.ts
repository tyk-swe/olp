import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { listGroups } from './api.js';

describe('listGroups', () => {
  it('rejects when the SCIM groups list repeats a cursor', async () => {
    const handler = vi.fn((request: Request) => {
      route(request, 'GET', '/api/v1/scim/groups');
      return json({
        items: [],
        next_cursor: handler.mock.calls.length <= 2 ? 'same-page' : null
      });
    });
    serve(handler, ['read']);

    await expect(listGroups()).rejects.toMatchObject({
      problem: { type: 'urn:olp:problem:invalid-cursor-cycle' }
    });
  });
});
