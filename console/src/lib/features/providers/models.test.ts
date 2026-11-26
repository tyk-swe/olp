import { describe, expect, it } from 'vitest';
import { PROVIDER_PAGE_SIZE } from '#lib/management/pageSizes.js';
import { json, route, serve } from '#lib/session/test/transport.js';
import type { Provider } from './providers';
import {
  certifyProviderModel,
  compareProviderListing,
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

describe('provider model API', () => {
  it.each([['one-model'], ['first-model', 'second-model']])(
    'declares identifiers using the discovery request contract, %j',
    async (...names: string[]) => {
      serve(async (request) => {
        route(request, 'POST', `/api/v1/providers/${provider.id}/discovery`);
        expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
        expect(await request.json()).toEqual({
          models: names.map((name) => ({
            upstream_model: name,
            display_name: name
          }))
        });
        return json(provider);
      });
      expect(await declareProviderModels(provider, names)).toEqual(provider);
    }
  );

  it('requests automatic discovery with an empty model list', async () => {
    serve(async (request) => {
      route(request, 'POST', `/api/v1/providers/${provider.id}/discovery`);
      expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
      expect(await request.json()).toEqual({ models: [] });
      return json(provider);
    });
    expect(await discoverProviderModels(provider)).toEqual(provider);
  });

  it('compares the upstream listing without a draft precondition', async () => {
    const comparison = {
      provider_id: provider.id,
      revision_id: 'revision-fixture',
      revision: 2,
      status: 'incomplete',
      detail: 'The final page was unavailable.',
      checked_at: new Date(0).toISOString(),
      api_version: '2023-06-01',
      credential_version: 3,
      new_models: [],
      changed_display_names: [],
      not_listed: [],
      unverified: []
    };
    serve(async (request) => {
      route(
        request,
        'POST',
        `/api/v1/providers/${provider.id}/listing-comparison`
      );
      expect(request.headers.get('If-Match')).toBeNull();
      return json(comparison);
    });
    expect(await compareProviderListing(provider.id)).toEqual(comparison);
  });

  it('surfaces rejected model declarations', async () => {
    serve(() => json({ title: 'Invalid model', status: 422 }, 422));
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
    serve((request) => {
      expect(request.signal.aborted).toBe(true);
      if (new URL(request.url).pathname === '/api/v1/provider-kinds') {
        route(request, 'GET', '/api/v1/provider-kinds');
        return json({ items: kinds });
      }
      route(
        request,
        'GET',
        `/api/v1/provider-kinds/${provider.configuration.kind}/capabilities`
      );
      return json(options);
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
    serve((request) => {
      route(request, 'GET', `/api/v1/providers/${provider.id}/models`);
      const url = new URL(request.url);
      expect(request.signal.aborted).toBe(true);
      expect(url.searchParams.get('limit')).toBe(String(PROVIDER_PAGE_SIZE));
      expect(url.searchParams.get('cursor')).toBe('previous-page');
      return json({
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
    serve((request) => {
      route(request, 'GET', '/api/v1/provider-models');
      const url = new URL(request.url);
      expect(request.signal.aborted).toBe(true);
      expect(Object.fromEntries(url.searchParams)).toEqual({
        limit: String(PROVIDER_PAGE_SIZE),
        cursor: 'previous-page',
        enabled: 'false',
        search: 'fixture',
        surface: 'openai'
      });
      return json({ items: [item], next_cursor: 'next-page' });
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
    serve((request) => {
      route(request, 'GET', '/api/v1/provider-models');
      const url = new URL(request.url);
      expect(url.searchParams.get('enabled')).toBe('true');
      expect(request.signal.aborted).toBe(true);
      if (url.searchParams.has('cursor')) {
        expect(url.searchParams.get('cursor')).toBe('next-page');
        return json({ items: [second], next_cursor: null });
      }
      return json({ items: [first], next_cursor: 'next-page' });
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
    serve(async (request) => {
      route(
        request,
        'PATCH',
        `/api/v1/providers/${provider.id}/models/${model.id}`
      );
      expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
      expect(await request.json()).toEqual({ enabled: true, capabilities });
      return json(provider);
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
    serve((request) => {
      route(
        request,
        'POST',
        `/api/v1/providers/${provider.id}/models/${model.id}/certify`
      );
      expect(request.signal.aborted).toBe(true);
      expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
      return Response.json(result, { headers: { ETag: 'certified-version' } });
    });
    expect(
      await certifyProviderModel(provider, model.id, controller.signal)
    ).toEqual({ result, etag: 'certified-version' });
  });

  it.each([undefined, '   '])(
    'rejects certification without a usable provider version, %j',
    async (etag) => {
      serve(() =>
        Response.json({}, { headers: etag === undefined ? {} : { ETag: etag } })
      );
      await expect(certifyProviderModel(provider, model.id)).rejects.toThrow(
        'Refresh before continuing'
      );
    }
  );
});
