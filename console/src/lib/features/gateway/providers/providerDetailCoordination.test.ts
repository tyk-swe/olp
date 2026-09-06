import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { isEtagMismatch } from '$lib/api/http';
import type { Provider } from '$lib/api/management/providers';
import { queryKeys } from '$lib/api/queryKeys';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  fetchCoordinatedModelPage,
  installProviderWithModels,
  providerModelPageKey
} from './providerDetailCoordination';

const provider = { id: 'provider-id', etag: 'etag-7' } as Provider;

afterEach(() => vi.unstubAllGlobals());

describe('provider model page coordination', () => {
  it('pins each page to its required provider version', () => {
    expect(providerModelPageKey(provider, 'next-page')).toEqual([
      'provider-model-page',
      'provider-id',
      'next-page',
      'etag-7'
    ]);
    expect(
      providerModelPageKey({ ...provider, etag: 'etag-8' }, undefined)
    ).not.toEqual(providerModelPageKey(provider, undefined));
  });

  it('retains the authoritative page version and forwards its cursor and cancellation', async () => {
    const requests = captureRequests(() =>
      jsonResponse({
        provider_etag: provider.etag,
        items: [],
        next_cursor: 'page-three'
      })
    );
    const controller = new AbortController();
    const coordinated = await fetchCoordinatedModelPage(
      provider,
      'page-two',
      controller.signal
    );
    expect(coordinated).toEqual({
      provider,
      page: { providerEtag: provider.etag, items: [], nextCursor: 'page-three' }
    });
    expect(new URL(requests[0].url).searchParams.get('cursor')).toBe(
      'page-two'
    );
    controller.abort();
    expect(requests[0].signal.aborted).toBe(true);
  });

  it('installs the matching cache before changing pagination or exposing its provider', async () => {
    captureRequests(() =>
      jsonResponse({
        provider_etag: provider.etag,
        items: [],
        next_cursor: null
      })
    );
    const client = new QueryClient();
    const wizardKey = queryKeys.providers.models(provider.id);
    client.setQueryData(wizardKey, { items: [] });
    const events: string[] = [];
    await installProviderWithModels(
      client,
      provider,
      undefined,
      (accepted) => {
        expect(accepted).toBe(provider);
        expect(
          client.getQueryData(providerModelPageKey(provider, undefined))
        ).toMatchObject({ provider });
        events.push('accept');
      },
      () => {
        expect(
          client.getQueryData(providerModelPageKey(provider, undefined))
        ).toBeDefined();
        events.push('reset');
      }
    );
    expect(events).toEqual(['reset', 'accept']);
    expect(client.getQueryState(wizardKey)?.isInvalidated).toBe(true);
    client.clear();
  });

  it('rejects a changed page without caching it, resetting pagination or accepting its provider', async () => {
    captureRequests(() =>
      jsonResponse({ provider_etag: 'etag-8', items: [], next_cursor: null })
    );
    const client = new QueryClient();
    const oldPage = { provider, page: { items: ['retained'] } };
    client.setQueryData(providerModelPageKey(provider, undefined), oldPage);
    const accept = vi.fn();
    const reset = vi.fn();
    const error = await installProviderWithModels(
      client,
      provider,
      'next-page',
      accept,
      reset
    ).catch((error) => error);
    expect(isEtagMismatch(error)).toBe(true);
    expect(accept).not.toHaveBeenCalled();
    expect(reset).not.toHaveBeenCalled();
    expect(
      client.getQueryData(providerModelPageKey(provider, 'next-page'))
    ).toBeUndefined();
    expect(client.getQueryData(providerModelPageKey(provider, undefined))).toBe(
      oldPage
    );
    client.clear();
  });

  it('preserves model page transport failures', async () => {
    captureRequests(() =>
      jsonResponse(
        { title: 'Model page unavailable', status: 503 },
        { status: 503 }
      )
    );
    await expect(
      fetchCoordinatedModelPage(provider, undefined)
    ).rejects.toThrow('Model page unavailable');
  });
});
