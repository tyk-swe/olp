import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountState, mountWithState } from '#test/mount.js';
import type { BillingImport } from './billing';
import BillingImportSection from './BillingImportSection.svelte';
import { networkCredentialPath } from './configurationDraft.svelte';
import type {
  ProviderKindCapability,
  Provider
} from '#lib/domain/providers/api.js';
import { ProviderEditor } from './providerEditor.svelte';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';

const provider: Provider = {
  id: 'provider',
  etag: 'original',
  name: 'Service',
  configuration: {
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://provider.example.test'
  },
  capability_count: 0,
  certified_capability_count: 0,
  enabled_model_count: 0,
  model_count: 0,
  connector_ready: false,
  pending_activation: true,
  project_id: null,
  project_name: null,
  state: 'draft',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'API key', credential: 'required' }],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: []
};

async function providerEditorState() {
  const { value: state } = mountState(
    () => new ProviderEditor(() => provider.id),
    {
      configureClient: (client) =>
        client.setQueryDefaults(providerKeys.modelsOf(provider.id), {
          retry: false
        })
    }
  );
  await eventually(() => expect(state.view.draft.name).toBe(provider.name));
  return state;
}

it('retains a confirmed activation receipt and retries only reads after a refresh failure', async () => {
  const activate = vi.fn();
  let refuseRead = false;
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
      if (path.endsWith('/activate')) {
        activate(request.headers.get('Idempotency-Key'));
        refuseRead = true;
        return json({ runtime_generation: { sequence: 3 } });
      }
      if (refuseRead) return json({ message: 'Read unavailable' }, 503);
      return json({ provider, items: [], next_cursor: null });
    },
    ['read', 'configure']
  );
  const state = await providerEditorState();
  await state.activate();
  expect(state.view.refreshPending).toBe(true);
  expect(state.view.notice).toContain('runtime generation 3');
  expect(state.view.refreshError).not.toBe('');
  await state.activate();
  expect(activate).toHaveBeenCalledTimes(1);
  refuseRead = false;
  await state.retryRefresh();
  expect(state.view.refreshPending).toBe(false);
  expect(state.view.refreshError).toBe('');
  expect(activate).toHaveBeenCalledTimes(1);
});

it('keeps an uncertain activation request identity when retrying', async () => {
  const activate = vi.fn();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
      if (path.endsWith('/activate')) {
        activate(
          request.headers.get('Idempotency-Key'),
          request.headers.get('If-Match')
        );
        if (activate.mock.calls.length === 1)
          return json({ message: 'Uncertain response' }, 503);
        return json({ runtime_generation: { sequence: 4 } });
      }
      return json({ provider, items: [], next_cursor: null });
    },
    ['read', 'configure']
  );
  const state = await providerEditorState();
  await state.activate();
  await state.activate();
  expect(activate).toHaveBeenCalledTimes(2);
  expect(activate.mock.calls[0][0]).toBeTruthy();
  expect(activate.mock.calls[1]).toEqual(activate.mock.calls[0]);
});

it('preserves a dirty draft while acknowledging its own network credential write', async () => {
  let etag = provider.etag;
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
      if (path.endsWith('/network-credentials') && request.method === 'POST') {
        etag = 'credential-saved';
        return json({ credential_id: 'network-credential', etag });
      }
      return json({
        provider: { ...provider, etag },
        items: [],
        next_cursor: null
      });
    },
    ['read', 'configure']
  );
  const state = await providerEditorState();
  state.view.draft.name = 'Unsaved name';
  state.touch();
  await state.stageNetworkCredential('{"password":"secret"}');
  expect(state.view.draft.name).toBe('Unsaved name');
  expect(state.view.draft.document?.text(networkCredentialPath)).toBe(
    'network-credential'
  );
  expect(state.view.dirty).toBe(true);
  expect(state.view.concurrentNotice).toBeNull();
});

it('uses the reloaded version and a fresh request identity after explicitly discarding an uncertain activation', async () => {
  let etag = provider.etag;
  const activate = vi.fn();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
      if (path.endsWith('/activate')) {
        activate(
          request.headers.get('Idempotency-Key'),
          request.headers.get('If-Match')
        );
        if (activate.mock.calls.length === 1)
          return json({ message: 'Uncertain response' }, 503);
        return json({ runtime_generation: { sequence: 5 } });
      }
      return json({
        provider: { ...provider, etag },
        items: [],
        next_cursor: null
      });
    },
    ['read', 'configure']
  );
  const state = await providerEditorState();
  await state.activate();
  etag = 'reloaded-version';
  await state.reload();
  await state.activate();
  expect(activate).toHaveBeenCalledTimes(2);
  expect(activate.mock.calls[1][0]).not.toBe(activate.mock.calls[0][0]);
  expect(activate.mock.calls[1][1]).toBe(`"${etag}"`);
});

it('clears a billing import delete conflict after loading the new version', async () => {
  const initial: BillingImport = {
    provider_id: provider.id,
    source: 'openai_costs',
    enabled: true,
    scope: {},
    credential_configured: true,
    etag: 'initial',
    status: {
      next_import_at: null,
      last_import_at: null,
      last_success_at: null,
      last_error_code: null,
      imported_from: null,
      imported_through: null,
      source_revision: null
    }
  };
  const read = vi
    .fn(() => json({ ...initial, enabled: false, etag: 'refreshed' }))
    .mockReturnValueOnce(json(initial));
  serve(
    (request) => {
      if (request.method === 'GET') return read();
      route(
        request,
        'DELETE',
        `/api/v1/providers/${provider.id}/billing-import`
      );
      return json(
        {
          type: 'https://openllmproxy.dev/problems/etag_mismatch',
          title: 'The import changed',
          status: 412
        },
        412
      );
    },
    ['read', 'configure']
  );

  const { target } = mountWithState(
    BillingImportSection,
    {
      provider: {
        ...provider,
        configuration: { ...provider.configuration, kind: 'openai' }
      },
      canManage: true
    },
    { attach: true }
  );

  await eventually(() =>
    expect(findButton(target, 'Run now')).toHaveProperty('disabled', false)
  );
  findButton(target, 'Remove')?.click();
  await eventually(() => expect(findButton(document, 'Delete')).toBeDefined());
  findButton(document, 'Delete')?.click();
  await eventually(() => expect(findButton(target, 'Reload')).toBeDefined());
  findButton(target, 'Reload')?.click();
  await eventually(() => {
    expect(target.querySelector('dd')?.textContent).toBe('No');
    expect(target.querySelector('[role="alert"]')).toBeNull();
    expect(findButton(target, 'Run now')).toHaveProperty('disabled', true);
  });
  const enabled = target.querySelector<HTMLInputElement>(
    'input[type="checkbox"]'
  );
  enabled?.click();
  flushSync();
  expect(findButton(target, 'Run now')).toHaveProperty('disabled', true);
});
