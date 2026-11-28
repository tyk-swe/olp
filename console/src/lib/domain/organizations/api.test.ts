import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { listOrganizations, type Organization } from './api.js';

const organization: Organization = {
  id: 'organization-id',
  name: 'Operations',
  etag: 'organization-version',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_by: 'owner-id',
  project_count: 1
};

describe('organization reads', () => {
  it('loads every page of organizations', async () => {
    const second = { ...organization, id: 'another-organization' };
    const handler = vi.fn((request: Request) => {
      route(request, 'GET', '/api/v1/organizations');
      return new URL(request.url).searchParams.has('cursor')
        ? json({ items: [second], next_cursor: null })
        : json({ items: [organization], next_cursor: 'next-page' });
    });
    serve(handler, ['read']);

    await expect(listOrganizations()).resolves.toEqual([organization, second]);
    expect(
      new URL(handler.mock.calls[1][0].url).searchParams.get('cursor')
    ).toBe('next-page');
  });
});
