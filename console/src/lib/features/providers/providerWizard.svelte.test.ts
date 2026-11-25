import { useQueryClient } from '@tanstack/svelte-query';
import { providerKeys } from './providerKeys';
import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { authLifecycle } from '#lib/session/lifecycle.js';
import { json, route, serve } from '#lib/session/test/transport.js';
import type { ProviderKindCapability } from './models';
import type { Provider, ProviderProbe } from './providers';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import { networkCredentialPath } from './configurationDraft.svelte';
import BillingImportSection from './BillingImportSection.svelte';
import type { BillingImport } from './billing';
import { ProviderEditor } from './providerEditor.svelte';
import { ProviderWizardState } from './providerWizard.svelte';
import { eventually } from '../../../../tests/unit/support/eventually.js';

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible provider',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'API key', credential: 'required' }],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: []
};

const provider: Provider = {
  id: 'provider-id',
  etag: 'provider-version',
  name: 'Service',
  configuration: {
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://example.test/v1'
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

function probeResponse(succeeded: boolean): Response {
  const body: ProviderProbe = {
    checked_at: '2026-01-01T00:00:00Z',
    detail: succeeded ? 'Connection succeeded' : 'Connection failed',
    probe_type: 'connection',
    provider_id: provider.id,
    succeeded
  };
  return new Response(JSON.stringify(body), {
    headers: {
      'Content-Type': 'application/json',
      ETag: provider.etag
    }
  });
}

function serveWizard(
  probe = vi.fn(() => probeResponse(true)),
  override?: (request: Request) => Response | Promise<Response> | undefined
) {
  const create = vi.fn(async (request: Request) => {
    route(request, 'POST', '/api/v1/providers');
    expect(request.headers.get('Idempotency-Key')).toBeTruthy();
    expect(await request.json()).toMatchObject({
      name: 'Service',
      credential: 'secret',
      configuration: {
        kind: 'openai_compatible',
        endpoint: 'https://example.test/v1',
        auth_mode: 'api_key'
      }
    });
    return json({ id: provider.id });
  });
  const handler = vi.fn((request: Request) => {
    const response = override?.(request);
    if (response) return response;
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
    if (path === '/api/v1/providers' && request.method === 'POST')
      return create(request);
    if (path === `/api/v1/providers/${provider.id}/models`)
      return json({ items: [], provider });
    if (path === `/api/v1/providers/${provider.id}/probe`) {
      route(request, 'POST', path);
      expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
      return probe();
    }
    if (path === '/api/v1/provider-kinds/openai_compatible/capabilities')
      return json({ provider_kind: spec.kind, capabilities: [] });
    return json({ title: `Unexpected request ${request.method} ${path}` }, 404);
  });
  serve(handler, ['read', 'configure']);
  return { create, handler, probe };
}

async function wizard(): Promise<ProviderWizardState> {
  const ready = vi.fn((state: ProviderWizardState) => state);
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(new ProviderWizardState()) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  const state = ready.mock.results[0].value;
  await eventually(() => expect(state.draft).not.toBeNull());
  return state;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('provider wizard', () => {
  it('shows an invalid external credential error without submitting', async () => {
    const { create } = serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.useExternalCredential = true;
    draft.externalCredentialReference = '{broken';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(state.errorMessage).toBe(
      'Enter a JSON object with store, secret_id and immutable version.'
    );
    expect(create).not.toHaveBeenCalled();
    expect(state.wizardStep).toBe(1);
  });

  it('saves, clears the pasted credential and tests the connection', async () => {
    const { create, probe } = serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledOnce();
    expect(probe).toHaveBeenCalledOnce();
    expect(draft.credential).toBe('');
    expect(state.providerId).toBe(provider.id);
    expect(state.probe?.succeeded).toBe(true);
    expect(state.wizardStep).toBe(2);
  });

  it('retries a failed connection test without creating the saved provider again', async () => {
    const probe = vi
      .fn(() => probeResponse(true))
      .mockReturnValueOnce(probeResponse(false));
    const { create } = serveWizard(probe);
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Connection failed');
    expect(state.wizardStep).toBe(1);
    expect(draft.credential).toBe('');
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledOnce();
    expect(probe).toHaveBeenCalledTimes(2);
    expect(state.errorMessage).toBe('');
    expect(state.wizardStep).toBe(2);
  });
  it('retries an uncertain save with the original values and request identity', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      json({ title: 'Unavailable', status: 503 }, 503)
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Unavailable');
    draft.name = 'Edited';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toContain('Retry its original fields');
    expect(create).toHaveBeenCalledOnce();

    draft.name = 'Service';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(create).toHaveBeenCalledTimes(2);
    expect(create.mock.calls[0][0].headers.get('Idempotency-Key')).toBeTruthy();
    expect(create.mock.calls[1][0].headers.get('Idempotency-Key')).toBe(
      create.mock.calls[0][0].headers.get('Idempotency-Key')
    );
    expect(state.wizardStep).toBe(2);
  });

  it('lets a rejected save be corrected with a fresh request identity', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      json({ title: 'Invalid credential', status: 400 }, 400)
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'invalid';

    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    expect(state.errorMessage).toBe('Invalid credential');
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));

    expect(create).toHaveBeenCalledTimes(2);
    expect(create.mock.calls[0][0].headers.get('Idempotency-Key')).toBeTruthy();
    expect(create.mock.calls[1][0].headers.get('Idempotency-Key')).not.toBe(
      create.mock.calls[0][0].headers.get('Idempotency-Key')
    );
    expect(state.wizardStep).toBe(2);
  });

  it('restores the saved connection when going back and clears secrets on reset', async () => {
    serveWizard();
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    flushSync();

    state.goBack();
    expect(state.wizardStep).toBe(1);
    expect(state.draft).toMatchObject({
      name: 'Service',
      endpoint: 'https://example.test/v1',
      credential: ''
    });
    const previous = state.draft;
    expect(previous).not.toBeNull();
    if (!previous) return;
    previous.credential = 'replacement';
    previous.externalCredentialReference = 'reference';
    state.startAnother();
    flushSync();

    expect(previous.credential).toBe('');
    expect(previous.externalCredentialReference).toBe('');
    expect(state.providerId).toBe('');
    expect(state.wizardStep).toBe(1);
    expect(state.probe).toBeNull();
    await eventually(() => expect(state.draft?.name).toBe(''));
  });

  it('offers manual model declaration when discovery returns no models', async () => {
    const payload = vi.fn<(body: unknown) => void>();
    const discovery = vi.fn(async (request: Request) => {
      route(request, 'POST', `/api/v1/providers/${provider.id}/discovery`);
      payload(await request.json());
      return json({
        ...provider,
        model_count: payload.mock.calls.length === 1 ? 0 : 2
      });
    });
    serveWizard(undefined, (request) =>
      new URL(request.url).pathname.endsWith('/discovery')
        ? discovery(request)
        : undefined
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await eventually(() => expect(state.wizardProvider?.id).toBe(provider.id));

    await state.discoverWizardProvider();
    expect(state.errorMessage).toContain('Use the manual identifier fallback');
    state.manualModelNames = 'model-a\nmodel-b';
    await state.declareWizardModels();

    expect(discovery).toHaveBeenCalledTimes(2);
    expect(payload).toHaveBeenNthCalledWith(1, { models: [] });
    expect(payload).toHaveBeenNthCalledWith(2, {
      models: [
        { upstream_model: 'model-a', display_name: 'model-a' },
        { upstream_model: 'model-b', display_name: 'model-b' }
      ]
    });
    expect(state.errorMessage).toBe('');
    expect(state.manualModelNames).toBe('');
    expect(state.wizardStep).toBe(2);
  });

  it('retries an uncertain activation before allowing connection changes', async () => {
    const activate = vi
      .fn((request: Request) => {
        route(request, 'POST', `/api/v1/providers/${provider.id}/activate`);
        return json({ runtime_generation: { sequence: 42 } });
      })
      .mockReturnValueOnce(json({ title: 'Unavailable', status: 503 }, 503));
    serveWizard(undefined, (request) =>
      new URL(request.url).pathname.endsWith('/activate')
        ? activate(request)
        : undefined
    );
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await eventually(() => expect(state.wizardProvider?.id).toBe(provider.id));

    await state.activateWizardProvider();
    expect(state.errorMessage).toBe('Unavailable');
    state.goBack();
    expect(state.errorMessage).toContain(
      'Activation may already have committed'
    );
    expect(state.wizardStep).toBe(2);
    await state.activateWizardProvider();

    expect(activate).toHaveBeenCalledTimes(2);
    const firstId = activate.mock.calls[0][0].headers.get('Idempotency-Key');
    expect(firstId).toBeTruthy();
    expect(activate.mock.calls[1][0].headers.get('Idempotency-Key')).toBe(
      firstId
    );
    expect(state.wizardStep).toBe(3);
    expect(state.notice).toContain('runtime generation 42');
  });
  it('retries refresh after confirmed activation without activating twice', async () => {
    let activated = false;
    let refreshUnavailable = true;
    const activate = vi.fn(() => {
      activated = true;
      return json({ runtime_generation: { sequence: 42 } });
    });
    serveWizard(undefined, (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/activate')) return activate();
      if (activated && refreshUnavailable && path.endsWith('/models'))
        return json({ title: 'Refresh unavailable', status: 503 }, 503);
      return undefined;
    });
    const state = await wizard();
    const draft = state.draft;
    expect(draft).not.toBeNull();
    if (!draft) return;
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.credential = 'secret';
    await state.createDraft(new SubmitEvent('submit', { cancelable: true }));
    await state.activateWizardProvider();
    expect(state.errorMessage).toContain('Refresh unavailable');
    refreshUnavailable = false;
    await state.activateWizardProvider();
    expect(activate).toHaveBeenCalledTimes(1);
    expect(state.wizardStep).toBe(3);
    expect(state.errorMessage).toBe('');
  });
});

const editorProvider: Provider = {
  id: 'provider',
  etag: 'original',
  name: 'Service',
  configuration: {
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://editorProvider.example.test'
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
const editorSpec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'API key', credential: 'required' }],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: []
};

async function providerEditorState(
  providerId: () => string = () => editorProvider.id
) {
  const ready = vi.fn(() => {
    useQueryClient().setQueryDefaults(
      providerKeys.modelsOf(editorProvider.id),
      { retry: false }
    );
    return new ProviderEditor(providerId);
  });
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: ready }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  const state = ready.mock.results[0].value;
  await eventually(() =>
    expect(state.view.draft.name).toBe(editorProvider.name)
  );
  return state;
}

it('retains a confirmed activation receipt and retries only reads after a refresh failure', async () => {
  const activate = vi.fn();
  let refuseRead = false;
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({ items: [editorSpec] });
      if (path.endsWith('/activate')) {
        activate(request.headers.get('Idempotency-Key'));
        refuseRead = true;
        return json({ runtime_generation: { sequence: 3 } });
      }
      if (refuseRead) return json({ message: 'Read unavailable' }, 503);
      return json({ provider: editorProvider, items: [], next_cursor: null });
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
      if (path === '/api/v1/provider-kinds')
        return json({ items: [editorSpec] });
      if (path.endsWith('/activate')) {
        activate(
          request.headers.get('Idempotency-Key'),
          request.headers.get('If-Match')
        );
        if (activate.mock.calls.length === 1)
          return json({ message: 'Uncertain response' }, 503);
        return json({ runtime_generation: { sequence: 4 } });
      }
      return json({ provider: editorProvider, items: [], next_cursor: null });
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
  let etag = editorProvider.etag;
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({ items: [editorSpec] });
      if (path.endsWith('/network-credentials') && request.method === 'POST') {
        etag = 'credential-saved';
        return json({ credential_id: 'network-credential', etag });
      }
      return json({
        provider: { ...editorProvider, etag },
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
  let etag = editorProvider.etag;
  const activate = vi.fn();
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({ items: [editorSpec] });
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
        provider: { ...editorProvider, etag },
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

function serveEditorWrite(write: (request: Request) => Promise<Response>) {
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({ items: [editorSpec] });
      if (request.method !== 'GET') return write(request);
      const id = path.split('/')[4];
      return json({
        provider: { ...editorProvider, id },
        items: [],
        next_cursor: null
      });
    },
    ['read', 'configure']
  );
}

const problem = (type: string, status: number, extra: object = {}) =>
  json(
    {
      type: `https://openllmproxy.dev/problems/${type}`,
      title: 'Refused',
      status,
      ...extra
    },
    status
  );

it('names the running provider write, sends one at a time and replaces the last failure', async () => {
  const answers = [
    Promise.withResolvers<Response>(),
    Promise.withResolvers<Response>()
  ];
  const write = vi.fn(() => answers[write.mock.calls.length - 1].promise);
  serveEditorWrite(write);
  const state = await providerEditorState();

  const first = state.activate();
  await eventually(() => expect(write).toHaveBeenCalledOnce());
  expect(state.view.busy).toBe('detail-activate');
  await state.disable();
  expect(write).toHaveBeenCalledOnce();
  answers[0].resolve(
    problem('validation_failed', 422, {
      detail: 'The provider is not ready',
      errors: { name: [{ code: 'required', message: 'Name is required' }] }
    })
  );
  await first;
  expect(state.view.busy).toBe('');
  expect(state.view.error).toBe('The provider is not ready');
  expect(state.view.validationIssues).toEqual([
    expect.objectContaining({ message: 'Name is required' })
  ]);

  const second = state.activate();
  await eventually(() => expect(write).toHaveBeenCalledTimes(2));
  expect(state.view.error).toBe('');
  expect(state.view.validationIssues).toEqual([]);
  answers[1].resolve(json({ runtime_generation: { sequence: 6 } }));
  await second;
  expect(state.view.notice).toContain('runtime generation 6');
});

it('flags a provider changed elsewhere without an error and keeps the flag until a reload', async () => {
  const write = vi.fn(async () => problem('etag_mismatch', 412));
  serveEditorWrite(write);
  const state = await providerEditorState();

  await state.activate();
  expect(state.view.concurrentNotice).not.toBeNull();
  expect(state.view.error).toBe('');
  expect(state.view.busy).toBe('');
  await state.reload();
  expect(state.view.concurrentNotice).toBeNull();
  expect(state.view.busy).toBe('');
});

it('says what still uses a provider that cannot be disabled until the next write starts', async () => {
  const write = vi
    .fn(async () => json({ runtime_generation: { sequence: 7 } }))
    .mockResolvedValueOnce(
      problem('configuration_resource_in_use', 409, {
        detail: 'A route still uses this provider'
      })
    );
  serveEditorWrite(write);
  const state = await providerEditorState();

  await state.disable();
  expect(state.view.referenceConflict).toBe('A route still uses this provider');
  expect(state.view.error).toBe('');
  await state.activate();
  expect(state.view.referenceConflict).toBe('');
});

it('ignores a provider write answered after another principal signed in', async () => {
  const answer = Promise.withResolvers<Response>();
  const write = vi.fn(() => answer.promise);
  serveEditorWrite(write);
  const state = await providerEditorState();

  const activating = state.activate();
  await eventually(() => expect(write).toHaveBeenCalledOnce());
  authLifecycle.establishSession({
    user: {
      id: 'another-owner',
      email: 'another@example.test',
      display_name: 'Another owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'fixture-csrf'
  });
  flushSync();
  expect(state.view.busy).toBe('');
  answer.resolve(json({ message: 'Refused' }, 500));
  await activating;
  expect(state.view.error).toBe('');
  expect(state.view.notice).toBe('');
});

it('ignores a provider write answered after the page moved to another provider', async () => {
  const answer = Promise.withResolvers<Response>();
  const write = vi.fn(() => answer.promise);
  serveEditorWrite(write);
  let providerId = $state(editorProvider.id);
  const state = await providerEditorState(() => providerId);

  const activating = state.activate();
  await eventually(() => expect(write).toHaveBeenCalledOnce());
  providerId = 'another-provider';
  flushSync();
  expect(state.view.busy).toBe('');
  answer.resolve(json({ runtime_generation: { sequence: 8 } }));
  await activating;
  expect(state.view.notice).toBe('');
  expect(state.view.refreshPending).toBe(false);
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

  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(BillingImportSection, {
      target,
      props: {
        provider: {
          ...provider,
          configuration: { ...provider.configuration, kind: 'openai' }
        },
        canManage: true
      },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    target.remove();
  });
  const button = (root: ParentNode, label: string) =>
    [...root.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === label
    );

  await eventually(() =>
    expect(button(target, 'Run now')).toHaveProperty('disabled', false)
  );
  button(target, 'Remove')?.click();
  await eventually(() => expect(button(document, 'Delete')).toBeDefined());
  button(document, 'Delete')?.click();
  await eventually(() => expect(button(target, 'Reload')).toBeDefined());
  button(target, 'Reload')?.click();
  await eventually(() => {
    expect(target.querySelector('dd')?.textContent).toBe('No');
    expect(target.querySelector('[role="alert"]')).toBeNull();
    expect(button(target, 'Run now')).toHaveProperty('disabled', true);
  });
  const enabled = target.querySelector<HTMLInputElement>(
    'input[type="checkbox"]'
  );
  enabled?.click();
  flushSync();
  expect(button(target, 'Run now')).toHaveProperty('disabled', true);
});
