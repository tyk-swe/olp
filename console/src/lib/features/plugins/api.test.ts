import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { operationsFor } from '$lib/features/access/session/test/grants';
import { clearCsrfToken } from '$lib/features/access/session/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { ApiProblem } from '$lib/api/http';
import {
  approvePlugin,
  installPlugin,
  listPlugins,
  pluginProblem,
  uninstallPlugin,
  type Plugin
} from '$lib/features/plugins/api';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'owner@example.com',
    display_name: 'Owner',
    role: 'owner' as const,
    access_scope: 'global' as const,
    operations: operationsFor('owner')
  },
  csrf_token: 'csrf-plugin-token'
};

const plugin: Plugin = {
  digest: 'b'.repeat(64),
  abi_version: 1,
  size_bytes: 8,
  executable: null,
  manifest: {
    name: 'acme',
    version: '1.0.0',
    origins: ['https://login.acme.example', 'https://api.acme.example'],
    profiles: [
      {
        id: 'acme-chat',
        label: 'Acme Chat',
        dialect: 'openai-chat',
        hosting: { address: 'https://api.acme.example/v1' }
      }
    ]
  },
  installed_by: '01980000-0000-7000-8000-000000000401',
  installed_by_email: 'owner@example.com',
  installed_at: '2026-09-27T06:00:00Z',
  approved_by: null,
  approved_by_email: null,
  approved_at: null,
  etag: '01980000-0000-7000-8000-000000000910'
};

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('plugin management api', () => {
  it('lists installed plugins', async () => {
    const listed = { items: [plugin], unconfined_plugins_enabled: false };
    const requests = captureRequests(() => jsonResponse(listed));
    expect(await listPlugins()).toEqual(listed);
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/plugins');
  });

  it('uploads the module bytes as application/wasm', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse(plugin, { status: 201 })
    );
    const bytes = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0]);

    const installed = await installPlugin(new Blob([bytes]));

    expect(installed).toEqual({ plugin, created: true });
    expect(requests[0]!.method).toBe('POST');
    expect(requests[0]!.headers.get('content-type')).toBe('application/wasm');
    expect(requests[0]!.headers.get('x-csrf-token')).toBe('csrf-plugin-token');
    expect(new Uint8Array(await requests[0]!.arrayBuffer())).toEqual(bytes);
  });

  it('reports an already installed digest as unchanged', async () => {
    authLifecycle.establishSession(session);
    captureRequests(() => jsonResponse(plugin));
    expect(await installPlugin(new Blob(['module']))).toEqual({
      plugin,
      created: false
    });
  });

  it('approves exactly the declared origins under the ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(plugin));

    await approvePlugin(plugin);

    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/plugins/${plugin.digest}/approve`
    );
    expect(requests[0]!.headers.get('if-match')).toBe(`"${plugin.etag}"`);
    expect(await requests[0]!.json()).toEqual({
      origins: plugin.manifest.origins
    });
  });

  it('uninstalls under the ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => new Response(null, { status: 204 }));

    await uninstallPlugin(plugin);

    expect(requests[0]!.method).toBe('DELETE');
    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/plugins/${plugin.digest}`
    );
    expect(requests[0]!.headers.get('if-match')).toBe(`"${plugin.etag}"`);
  });

  it('surfaces a typed refusal by its problem type and detail', async () => {
    authLifecycle.establishSession(session);
    captureRequests(() =>
      jsonResponse(
        {
          type: 'https://openllmproxy.dev/problems/plugin_abi_unsupported',
          title: 'Unprocessable Entity',
          status: 422,
          detail: 'The module was built for plugin ABI 2; this OLP runs ABI 1.'
        },
        { status: 422, headers: { 'content-type': 'application/problem+json' } }
      )
    );
    const refusal = await installPlugin(new Blob(['module'])).catch(
      (error: unknown) => error
    );
    expect(refusal).toBeInstanceOf(ApiProblem);
    expect((refusal as ApiProblem).problem.type).toMatch(
      /plugin_abi_unsupported$/
    );
    expect(pluginProblem(refusal)).toBe(
      'The module was built for plugin ABI 2; this OLP runs ABI 1.'
    );
  });
});
