import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '$lib/api/test/transport';
import { ApiProblem } from '$lib/api/http';
import * as api from './api';

const plugin: api.Plugin = {
  digest: 'abcdef0123456789abcdef',
  etag: 'build-tag',
  abi_version: 1,
  approved_at: null,
  approved_by: null,
  approved_by_email: null,
  executable: null,
  installed_at: '2026-10-22T00:00:00Z',
  installed_by: 'owner',
  installed_by_email: 'owner@example.test',
  size_bytes: 3,
  manifest: {
    name: 'Fixture plugin',
    version: '1',
    origins: ['https://tools.example.test'],
    profiles: []
  }
};
const review: api.UnconfinedExecutableReview = {
  name: 'fixture-executable',
  digest: plugin.digest,
  abi_version: 1,
  manifest: plugin.manifest,
  permitted: false,
  size_bytes: 3
};
const native: api.MCPExecutablePermission = {
  name: 'fixture-mcp',
  digest: 'fedcba9876543210',
  permitted: false,
  size_bytes: 4
};
afterEach(() => vi.unstubAllGlobals());

describe('plugin HTTP contracts', () => {
  it('reads installed plugins, the reviewed index and executable inventories', async () => {
    const controller = new AbortController();
    serve((request) => {
      route(request, 'GET', new URL(request.url).pathname);
      switch (new URL(request.url).pathname) {
        case '/api/v1/plugins':
          return json({ items: [plugin], unconfined_plugins_enabled: true });
        case '/api/v1/unconfined-plugins':
          return json({ items: [review] });
        case '/api/v1/plugin-index':
          return json({ entries: [{ name: 'reviewed' }] });
        case '/api/v1/mcp-executables':
          return json({ items: [native] });
        default:
          return json({ status: 404, title: 'Wrong collection' }, 404);
      }
    });
    expect(await api.listPlugins(controller.signal)).toEqual({
      items: [plugin],
      unconfined_plugins_enabled: true
    });
    expect(await api.listUnconfinedExecutables(controller.signal)).toEqual([
      review
    ]);
    expect(await api.getPluginIndex(controller.signal)).toEqual({
      entries: [{ name: 'reviewed' }]
    });
    expect(await api.listMCPExecutables(controller.signal)).toEqual([native]);
  });

  it.each([200, 201])(
    'uploads unchanged binary bytes and reports creation from HTTP %i',
    async (status) => {
      const bytes = new Uint8Array([0, 255, 7]);
      serve(async (request) => {
        route(request, 'POST', '/api/v1/plugins');
        expect(request.headers.get('Content-Type')).toBe('application/wasm');
        expect(new Uint8Array(await request.arrayBuffer())).toEqual(bytes);
        return json(plugin, status);
      });
      expect(await api.installPlugin(new Blob([bytes]))).toEqual({
        plugin,
        created: status === 201
      });
    }
  );

  it('approves exactly the declared origins and deletes the selected build conditionally', async () => {
    serve(async (request) => {
      expect(request.headers.get('If-Match')).toBe(plugin.etag);
      if (request.method === 'POST') {
        route(request, 'POST', `/api/v1/plugins/${plugin.digest}/approve`);
        expect(await request.json()).toEqual({
          origins: plugin.manifest.origins
        });
        return json({ ...plugin, approved_by: 'owner' });
      }
      route(request, 'DELETE', `/api/v1/plugins/${plugin.digest}`);
      return new Response(null, { status: 204 });
    });
    expect((await api.approvePlugin(plugin)).approved_by).toBe('owner');
    await expect(api.uninstallPlugin(plugin)).resolves.toBeUndefined();
    serve(() => json({ status: 412, title: 'Build changed' }, 412));
    await expect(api.uninstallPlugin(plugin)).rejects.toMatchObject({
      problem: { status: 412 }
    });
  });

  it('reviews a native plugin before acknowledging exactly its build', async () => {
    serve(async (request) => {
      route(
        request,
        'POST',
        `/api/v1/unconfined-plugins/${review.name}/review`
      );
      expect(request.body).toBeNull();
      return json(review);
    });
    expect(await api.reviewUnconfinedExecutable(review.name)).toEqual(review);
    serve(async (request) => {
      route(
        request,
        'POST',
        `/api/v1/unconfined-plugins/${review.name}/permit`
      );
      expect(await request.json()).toEqual({
        digest: review.digest,
        acknowledge_risk: true
      });
      return json({ ...plugin, executable: review.name });
    });
    expect((await api.permitUnconfinedPlugin(review)).executable).toBe(
      review.name
    );
  });

  it('reviews, permits and revokes exactly one native MCP executable digest', async () => {
    serve((request) => {
      route(request, 'POST', `/api/v1/mcp-executables/${native.name}/review`);
      expect(request.body).toBeNull();
      return json(native);
    });
    expect(await api.reviewMCPExecutable(native.name)).toEqual(native);
    serve(async (request) => {
      route(request, 'POST', `/api/v1/mcp-executables/${native.name}/permit`);
      expect(await request.json()).toEqual({
        digest: native.digest,
        acknowledge_risk: true
      });
      return json({ ...native, permitted: true });
    });
    expect((await api.permitMCPExecutable(native)).permitted).toBe(true);
    serve((request) => {
      route(
        request,
        'DELETE',
        `/api/v1/mcp-executables/${native.name}/permissions/${native.digest}`
      );
      return new Response(null, { status: 204 });
    });
    await expect(api.revokeMCPExecutable(native)).resolves.toBeUndefined();
    serve(() =>
      json(
        {
          status: 428,
          title: 'Authenticate again',
          detail: 'Confirm owner identity',
          errors: {
            digest: [
              { code: 'changed', message: 'Review the current executable' }
            ]
          }
        },
        428
      )
    );
    const error = await api
      .permitMCPExecutable(native)
      .catch((cause: unknown) => cause);
    expect(api.needsReauthentication(error)).toBe(true);
    expect(api.pluginProblem(error)).toBe('Confirm owner identity (digest)');
    await expect(api.revokeMCPExecutable(native)).rejects.toBeInstanceOf(
      ApiProblem
    );
    await expect(api.reviewMCPExecutable(native.name)).rejects.toBeInstanceOf(
      ApiProblem
    );
  });

  it('preserves bounded refusal messages and distinguishes authentication failures', async () => {
    serve(() => json({ status: 403, title: 'Permission required' }, 403));
    const error = await api.listPlugins().catch((cause: unknown) => cause);
    expect(api.needsReauthentication(error)).toBe(false);
    expect(api.needsReauthentication(new Error('offline'))).toBe(false);
    expect(api.pluginProblem(error)).toBe('Permission required');
    expect(api.pluginProblem(new Error('offline'))).toBe('offline');
    expect(api.pluginProblem(null)).toBe(
      'The control API could not complete the request.'
    );
    expect(api.pluginTitle(plugin)).toBe(
      `${plugin.manifest.name} ${plugin.manifest.version}`
    );
    expect(api.shortDigest(plugin.digest)).toBe(plugin.digest.substring(0, 12));
  });

  it.each([
    api.listPlugins,
    api.listUnconfinedExecutables,
    api.getPluginIndex,
    api.listMCPExecutables
  ])('passes cancellation through a read request', async (load) => {
    const controller = new AbortController();
    serve((request) => {
      controller.abort();
      expect(request.signal.aborted).toBe(true);
      return Promise.reject(new DOMException('Cancelled', 'AbortError'));
    });
    await expect(load(controller.signal)).rejects.toMatchObject({
      name: 'AbortError'
    });
  });
});
