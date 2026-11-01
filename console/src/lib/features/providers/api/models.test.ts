import { afterEach, describe, expect, it, vi } from 'vitest';
import { PROVIDER_PAGE_SIZE } from '$lib/api/pageSizes';
import type { Provider } from './providers';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  getProviderCapabilityOptions,
  listProviderKinds,
  listProviderModelInventory,
  listProviderModelInventoryPage,
  listProviderModelPage,
  setProviderModel,
  type ProviderModel
} from './models';

vi.mock('$lib/features/access/session/authMiddleware', () => ({
  createAuthMiddleware: () => ({
    onRequest: ({ request }: { request: Request }) => request
  })
}));

const provider: Provider = {
  id: 'provider-fixture',
  etag: 'provider-version',
  name: 'Fixture provider',
  configuration: { kind: 'openai_compatible', auth_mode: 'api_key' },
  state: 'draft',
  connector_ready: true,
  pending_activation: false,
  model_count: 0,
  enabled_model_count: 0,
  capability_count: 0,
  certified_capability_count: 0,
  project_id: null,
  project_name: null,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(0).toISOString()
};
const model: ProviderModel = {
  id: 'model-fixture',
  upstream_model: 'fixture-model',
  display_name: 'Fixture model',
  enabled: false,
  capabilities: []
};

function respond(handler: (request: Request) => Response | Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn(handler));
}

afterEach(() => vi.unstubAllGlobals());

describe('provider model API', () => {
  it.each([['one-model'], ['first-model', 'second-model']])(
    'declares identifiers using the discovery request contract, %j',
    async (...names: string[]) => {
      respond(async (request) => {
        expect(new URL(request.url).pathname).toBe(
          `/api/v1/providers/${provider.id}/discovery`
        );
        expect(request.method).toBe('POST');
        expect(request.headers.get('If-Match')).toBe(provider.etag);
        expect(await request.json()).toEqual({
          models: names.map((name) => ({
            upstream_model: name,
            display_name: name
          }))
        });
        return Response.json(provider);
      });
      expect(await declareProviderModels(provider, names)).toEqual(provider);
    }
  );

  it('requests automatic discovery with an empty model list', async () => {
    respond(async (request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/discovery`
      );
      expect(request.method).toBe('POST');
      expect(request.headers.get('If-Match')).toBe(provider.etag);
      expect(await request.json()).toEqual({ models: [] });
      return Response.json(provider);
    });
    expect(await discoverProviderModels(provider)).toEqual(provider);
  });

  it('surfaces rejected model declarations', async () => {
    respond(() =>
      Response.json({ title: 'Invalid model', status: 422 }, { status: 422 })
    );
    await expect(
      declareProviderModels(provider, ['invalid'])
    ).rejects.toMatchObject({
      problem: { status: 422, title: 'Invalid model' }
    });
  });

  it('loads capability options and provider kinds', async () => {
    const options = {
      provider_kind: provider.configuration.kind,
      capabilities: [
        { surface: 'openai', operation: 'generation', mode: 'unary' }
      ]
    };
    const kinds = [{ kind: provider.configuration.kind, label: provider.name }];
    const controller = new AbortController();
    controller.abort();
    respond((request) => {
      expect(request.method).toBe('GET');
      expect(request.signal.aborted).toBe(true);
      if (new URL(request.url).pathname === '/api/v1/provider-kinds') {
        return Response.json({ items: kinds });
      }
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/provider-kinds/${provider.configuration.kind}/capabilities`
      );
      return Response.json(options);
    });
    expect(
      await getProviderCapabilityOptions(
        provider.configuration.kind,
        controller.signal
      )
    ).toEqual(options);
    expect(await listProviderKinds(controller.signal)).toEqual(kinds);
  });

  it('loads model pages with their provider and continuation cursor', async () => {
    const controller = new AbortController();
    controller.abort();
    respond((request) => {
      const url = new URL(request.url);
      expect(request.method).toBe('GET');
      expect(request.signal.aborted).toBe(true);
      expect(url.pathname).toBe(`/api/v1/providers/${provider.id}/models`);
      expect(url.searchParams.get('limit')).toBe(String(PROVIDER_PAGE_SIZE));
      expect(url.searchParams.get('cursor')).toBe('previous-page');
      return Response.json({
        provider,
        items: [model],
        next_cursor: 'next-page'
      });
    });
    expect(
      await listProviderModelPage(
        provider.id,
        'previous-page',
        controller.signal
      )
    ).toEqual({
      provider,
      items: [model],
      nextCursor: 'next-page'
    });
  });

  it('filters model inventory pages and preserves continuation', async () => {
    const controller = new AbortController();
    controller.abort();
    const item = { provider_id: provider.id, model };
    respond((request) => {
      const url = new URL(request.url);
      expect(request.method).toBe('GET');
      expect(request.signal.aborted).toBe(true);
      expect(url.pathname).toBe('/api/v1/provider-models');
      expect(Object.fromEntries(url.searchParams)).toEqual({
        limit: String(PROVIDER_PAGE_SIZE),
        cursor: 'previous-page',
        enabled: 'false',
        search: 'fixture',
        surface: 'openai'
      });
      return Response.json({ items: [item], next_cursor: 'next-page' });
    });
    expect(
      await listProviderModelInventoryPage(
        'previous-page',
        false,
        controller.signal,
        'fixture',
        'openai'
      )
    ).toEqual({
      items: [item],
      nextCursor: 'next-page'
    });
  });

  it('collects subsequent inventory pages with the same filters and signal', async () => {
    const controller = new AbortController();
    controller.abort();
    const first = { provider_id: provider.id, model };
    const second = {
      provider_id: provider.id,
      model: { ...model, id: 'another-model' }
    };
    respond((request) => {
      const url = new URL(request.url);
      expect(url.pathname).toBe('/api/v1/provider-models');
      expect(url.searchParams.get('enabled')).toBe('true');
      expect(request.signal.aborted).toBe(true);
      if (url.searchParams.has('cursor')) {
        expect(url.searchParams.get('cursor')).toBe('next-page');
        return Response.json({ items: [second], next_cursor: null });
      }
      return Response.json({ items: [first], next_cursor: 'next-page' });
    });
    expect(await listProviderModelInventory(true, controller.signal)).toEqual([
      first,
      second
    ]);
  });

  it('saves reviewed model capabilities with the provider version', async () => {
    const capabilities = [
      { surface: 'openai', operation: 'generation', mode: 'unary' }
    ];
    respond(async (request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/models/${model.id}`
      );
      expect(request.method).toBe('PATCH');
      expect(request.headers.get('If-Match')).toBe(provider.etag);
      expect(await request.json()).toEqual({ enabled: true, capabilities });
      return Response.json(provider);
    });
    expect(
      await setProviderModel(provider, model.id, true, capabilities)
    ).toEqual(provider);
  });

  it('returns certification results with their committed provider version', async () => {
    const result = {
      provider_id: provider.id,
      model_id: model.id,
      status: 'certified'
    };
    const controller = new AbortController();
    controller.abort();
    respond((request) => {
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/providers/${provider.id}/models/${model.id}/certify`
      );
      expect(request.method).toBe('POST');
      expect(request.signal.aborted).toBe(true);
      expect(request.headers.get('If-Match')).toBe(provider.etag);
      return Response.json(result, { headers: { ETag: 'certified-version' } });
    });
    expect(
      await certifyProviderModel(provider, model.id, controller.signal)
    ).toEqual({ result, etag: 'certified-version' });
  });

  it.each([undefined, '   '])(
    'rejects certification without a usable provider version, %j',
    async (etag) => {
      respond(() =>
        Response.json({}, { headers: etag === undefined ? {} : { ETag: etag } })
      );
      await expect(certifyProviderModel(provider, model.id)).rejects.toThrow(
        'Refresh before continuing'
      );
    }
  );
});
