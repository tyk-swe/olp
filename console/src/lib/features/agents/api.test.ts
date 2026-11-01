import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '$lib/api/test/transport';
import { ApiProblem } from '$lib/api/http';
import * as api from './api';

const id = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
const publication: api.Publication = {
  id,
  project_id: 'project-a',
  name: 'Tools',
  kind: 'toolset',
  etag: 'before',
  revision_id: 'revision-a',
  retired: false,
  draft: { project_id: 'project-a', name: 'Tools', toolset: { tools: [] } }
};
const server: api.MCPServer = {
  id,
  project_id: 'project-a',
  name: 'Tools',
  transport: 'streamable_http',
  endpoint: 'https://tools.example.test/mcp',
  enabled: true,
  has_credential: false,
  etag: 'before',
  revision_id: 'revision-a',
  revision: 1,
  certified_at: '2026-10-22T00:00:00Z',
  retired_at: null,
  oauth: null,
  delegation: null,
  unit_price: null,
  tool_prices: {},
  calls_per_minute: 60,
  max_concurrency: 8,
  per_key_calls_per_minute: 60,
  per_key_max_concurrency: 8,
  catalog: { protocol_version: '2025-11-25', digest: 'digest-a', tools: [] }
};

afterEach(() => vi.unstubAllGlobals());

describe('publication HTTP contracts', () => {
  it.each(['toolsets', 'agents', 'prompts'] as const)(
    'reads every %s page and the selected revision',
    async (kind) => {
      serve((request) => {
        const url = new URL(request.url);
        if (url.pathname.endsWith('/revisions/revision-b')) {
          route(request, 'GET', `/api/v1/${kind}/${id}/revisions/revision-b`);
          return json({ revision_id: 'revision-b' });
        }
        if (url.pathname.endsWith(id)) {
          route(request, 'GET', `/api/v1/${kind}/${id}`);
          return json(publication);
        }
        route(request, 'GET', `/api/v1/${kind}`);
        expect(url.searchParams.get('limit')).toBe('200');
        const cursor = url.searchParams.get('cursor');
        expect([null, 'next-page']).toContain(cursor);
        return cursor === null
          ? json({ items: [publication], next_cursor: 'next-page' })
          : json({
              items: [{ ...publication, id: 'second' }],
              next_cursor: null
            });
      });
      expect((await api.listPublications(kind)).map((item) => item.id)).toEqual(
        [id, 'second']
      );
      expect(await api.getPublication(kind, id)).toEqual(publication);
      expect(await api.getRevision(kind, id, 'revision-b')).toEqual({
        revision_id: 'revision-b'
      });
    }
  );

  it.each(['toolsets', 'agents', 'prompts'] as const)(
    'creates, updates, publishes and retires %s with the correct write identity',
    async (kind) => {
      const input: api.Draft = { project_id: 'project-a', name: 'Changed' };
      serve(async (request) => {
        const path = new URL(request.url).pathname;
        expect(request.headers.get('Idempotency-Key')).toBe('write-identity');
        if (request.method === 'POST' && path === `/api/v1/${kind}`) {
          route(request, 'POST', path);
          expect(request.headers.has('If-Match')).toBe(false);
          expect(await request.json()).toEqual(input);
          return json(publication, 201);
        }
        expect(request.headers.get('If-Match')).toBe('"before"');
        if (path.endsWith('/publish')) {
          route(request, 'POST', `/api/v1/${kind}/${id}/publish`);
          return json({ ...publication, revision_id: 'published' });
        }
        route(request, request.method, `/api/v1/${kind}/${id}`);
        if (request.method === 'PUT') {
          expect(await request.json()).toEqual(input);
          return json({ ...publication, name: input.name });
        }
        expect(request.method).toBe('DELETE');
        return new Response(null, { status: 204 });
      });
      expect(
        await api.savePublication(kind, null, input, 'write-identity')
      ).toEqual(publication);
      expect(
        (await api.savePublication(kind, publication, input, 'write-identity'))
          .name
      ).toBe('Changed');
      expect(
        (await api.publish(kind, publication, 'write-identity')).revision_id
      ).toBe('published');
      await expect(
        api.retire(kind, publication, 'write-identity')
      ).resolves.toBeUndefined();
    }
  );

  it('updates the selected prompt label and preserves write refusals', async () => {
    serve(async (request) => {
      route(request, 'PUT', `/api/v1/prompts/${id}/labels/staging`);
      expect(request.headers.get('If-Match')).toBe('"before"');
      expect(request.headers.get('Idempotency-Key')).toBe('label-write');
      expect(await request.json()).toEqual({ revision_id: 'pinned' });
      return json(publication);
    });
    expect(
      await api.setLabel(publication, 'staging', 'pinned', 'label-write')
    ).toEqual(publication);
    serve(() =>
      json(
        {
          title: 'Changed elsewhere',
          detail: 'Refresh before changing this resource',
          status: 412
        },
        412
      )
    );
    await expect(
      api.setLabel(publication, 'production', 'pinned', 'label-write')
    ).rejects.toMatchObject({
      problem: { status: 412 },
      message: 'Refresh before changing this resource'
    });
    await expect(
      api.retire('prompts', publication, 'retire-write')
    ).rejects.toBeInstanceOf(ApiProblem);
  });
});

describe('server and execution HTTP contracts', () => {
  it.each([
    ['projects', api.listProjects],
    ['mcp-servers', api.listServers],
    ['mcp-user-grants', api.listMCPGrants]
  ] as const)(
    'collects %s pages without losing the cursor',
    async (collection, list) => {
      serve((request) => {
        route(request, 'GET', `/api/v1/${collection}`);
        const query = new URL(request.url).searchParams;
        expect(query.get('limit')).toBe('200');
        return query.has('cursor')
          ? json({ items: [{ id: 'second' }], next_cursor: null })
          : json({ items: [{ id: 'first' }], next_cursor: 'next-page' });
      });
      expect((await list()).map((item) => item.id)).toEqual([
        'first',
        'second'
      ]);
    }
  );

  it('reads and conditionally replaces exactly one server, preserving all submitted authorization data', async () => {
    const input: api.MCPServerUpdate = {
      name: 'Updated',
      endpoint: server.endpoint,
      enabled: true,
      delegation: {
        issuer: 'https://issuer.example.test',
        audience: 'audience'
      },
      delegation_private_key: 'private-fixture',
      oauth: null
    };
    serve(async (request) => {
      route(request, request.method, `/api/v1/mcp-servers/${id}`);
      if (request.method === 'GET') return json(server);
      expect(request.method).toBe('PUT');
      expect(request.headers.get('If-Match')).toBe('"before"');
      expect(request.headers.get('Idempotency-Key')).toBe('server-write');
      expect(await request.json()).toEqual(input);
      return json({ ...server, name: input.name, etag: 'after' });
    });
    expect(await api.getServer(id)).toEqual(server);
    expect(await api.saveServer(server, input, 'server-write')).toEqual({
      ...server,
      name: 'Updated',
      etag: 'after'
    });
    serve(() =>
      json({ status: 409, title: 'Write identity already used' }, 409)
    );
    await expect(
      api.saveServer(server, input, 'server-write')
    ).rejects.toMatchObject({ problem: { status: 409 } });
  });

  it('keeps project, key and cursor filters on execution history and omits blank filters', async () => {
    serve((request) => {
      route(request, 'GET', '/api/v1/mcp-tool-calls');
      expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({
        project_id: 'project-a',
        api_key_id: 'key-a',
        cursor: 'after-a',
        limit: '50'
      });
      return json({ items: [{ id: 'attempt-a' }], next_cursor: 'after-b' });
    });
    expect(await api.listToolCalls('project-a', 'key-a', 'after-a')).toEqual({
      items: [{ id: 'attempt-a' }],
      nextCursor: 'after-b'
    });
    serve((request) => {
      route(request, 'GET', '/api/v1/mcp-tool-calls');
      expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({
        limit: '50'
      });
      return json({ items: [], next_cursor: null });
    });
    expect(await api.listToolCalls('', '')).toEqual({
      items: [],
      nextCursor: null
    });
    serve((request) => {
      route(request, 'GET', '/api/v1/execution-debits');
      expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({
        api_key_id: 'key-a',
        cursor: 'after-a',
        limit: '50'
      });
      return json({ items: [{ id: 'debit-a' }], next_cursor: 'after-b' });
    });
    expect(await api.listExposures('key-a', 'after-a')).toEqual({
      items: [{ id: 'debit-a' }],
      nextCursor: 'after-b'
    });
    serve((request) => {
      expect(Object.fromEntries(new URL(request.url).searchParams)).toEqual({
        limit: '50'
      });
      return json({ items: [], next_cursor: null });
    });
    expect(await api.listExposures('')).toEqual({
      items: [],
      nextCursor: null
    });
  });
});

describe('personal grant HTTP journey', () => {
  it('binds enrollment, browser and device continuation to their original identities', async () => {
    serve(async (request) => {
      route(request, 'POST', '/api/v1/mcp-servers/server-a/grant-enrollments');
      expect(await request.json()).toEqual({ api_key_id: 'key-a' });
      return json({
        enrollment_id: 'enrollment-a',
        url: '/agents/grants#enrollment-a.token',
        expires_at: 'expires'
      });
    });
    expect((await api.startMCPGrant('server-a', 'key-a')).enrollment_id).toBe(
      'enrollment-a'
    );
    serve(async (request) => {
      route(request, 'POST', '/api/v1/mcp-grant-enrollments/claim');
      expect(await request.json()).toEqual({
        enrollment_id: 'enrollment-a',
        handoff_token: 'token'
      });
      return json({ state: 'pending', enrollment: { id: 'enrollment-a' } });
    });
    expect((await api.claimMCPGrant('enrollment-a', 'token')).state).toBe(
      'pending'
    );
    for (const flow of ['authorization_code', 'device_code'] as const) {
      serve(async (request) => {
        route(
          request,
          'POST',
          '/api/v1/mcp-grant-enrollments/enrollment-a/authorize'
        );
        expect(await request.json()).toEqual({ flow });
        return json({
          state: 'pending',
          authorization_url: `https://identity.example.test/${flow}`
        });
      });
      expect(
        (await api.authorizeMCPGrant('enrollment-a', flow)).authorization_url
      ).toBe(`https://identity.example.test/${flow}`);
    }
    serve(async (request) => {
      route(
        request,
        'POST',
        '/api/v1/mcp-grant-enrollments/enrollment-a/complete'
      );
      expect(await request.json()).toEqual({
        code: 'one-use-code',
        state: 'exact-state'
      });
      return json({ state: 'active' });
    });
    expect(
      (
        await api.completeMCPGrant(
          'enrollment-a',
          'one-use-code',
          'exact-state'
        )
      ).state
    ).toBe('active');
    serve((request) => {
      route(request, 'POST', '/api/v1/mcp-grant-enrollments/enrollment-a/poll');
      expect(request.body).toBeNull();
      return json({ state: 'denied' });
    });
    expect((await api.pollMCPGrant('enrollment-a')).state).toBe('denied');
    serve((request) => {
      route(request, 'DELETE', '/api/v1/mcp-user-grants/grant-a');
      return json({ state: 'revoked' });
    });
    expect(await api.revokeMCPGrant('grant-a')).toEqual({ state: 'revoked' });
  });

  it('propagates one-use, foreign-user and provider refusals instead of returning successful standing', async () => {
    serve(() => json({ status: 409, title: 'Enrollment already used' }, 409));
    await expect(api.claimMCPGrant('used', 'token')).rejects.toMatchObject({
      problem: { status: 409 }
    });
    serve(() =>
      json({ status: 403, title: 'Enrollment belongs to another user' }, 403)
    );
    await expect(
      api.completeMCPGrant('foreign', 'code', 'state')
    ).rejects.toMatchObject({ problem: { status: 403 } });
    serve(() => json({ status: 422, title: 'Authorization refused' }, 422));
    await expect(api.pollMCPGrant('denied')).rejects.toMatchObject({
      problem: { status: 422 }
    });
    await expect(api.revokeMCPGrant('foreign')).rejects.toBeInstanceOf(
      ApiProblem
    );
    await expect(
      api.authorizeMCPGrant('foreign', 'device_code')
    ).rejects.toBeInstanceOf(ApiProblem);
    await expect(api.startMCPGrant('foreign', 'key')).rejects.toBeInstanceOf(
      ApiProblem
    );
  });
});
