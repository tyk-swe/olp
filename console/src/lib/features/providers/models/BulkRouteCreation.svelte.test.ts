import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import type { ProviderModelInventory } from '../models';
import BulkRouteCreation from './BulkRouteCreation.svelte';

const model: ProviderModelInventory = {
  provider_id: 'provider-12345678',
  provider_name: 'Primary',
  provider_kind: 'openai',
  available: true,
  metadata: {
    canonical_model: null,
    context_length: null,
    data_collection: null,
    deployment: null,
    input_modalities: [],
    max_output_tokens: null,
    observed_at: null,
    output_modalities: [],
    quantization: null,
    region: null,
    source: null,
    supported_parameters: null,
    zero_data_retention: null
  },
  model: {
    id: 'model-a',
    upstream_model: 'test-model',
    display_name: 'Test model',
    enabled: true,
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'unary',
        source: 'certified'
      }
    ]
  }
};

let host: HTMLElement;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  component = mount(BulkRouteCreation, {
    target: host,
    props: { models: [model], canManage: true }
  });
  flushSync();
});

afterEach(async () => {
  await unmount(component);
  host.remove();
  vi.restoreAllMocks();
});

it.each([false, true])(
  'publishes only the created draft version (concurrent edit: %s)',
  async (changed) => {
    const receipt = {
      id: 'draft-a',
      slug: 'test-model-12345678',
      etag: 'reviewed-etag',
      state: 'draft'
    };
    const post = vi
      .spyOn(apiClient, 'POST')
      .mockResolvedValueOnce({
        data: receipt,
        response: new Response(null, { status: 201 })
      } as never)
      .mockResolvedValue(
        changed
          ? ({
              error: {
                type: 'https://openllmproxy.dev/problems/etag_mismatch',
                title: 'Draft changed',
                status: 412
              },
              response: new Response(null, { status: 412 })
            } as never)
          : ({
              data: { route_id: 'route-a', revision: 1 },
              response: new Response(null, { status: 200 })
            } as never)
      );
    const get = vi.spyOn(apiClient, 'GET').mockResolvedValue({
      data: { ...receipt, etag: 'unreviewed-etag' },
      response: new Response(null, { status: 200 })
    } as never);
    host.querySelector<HTMLInputElement>('input[type=checkbox]')!.click();
    flushSync();
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    const publish = () =>
      [...host.querySelectorAll('button')].find(
        (button) =>
          button.textContent?.trim() === 'Validate and publish reviewed routes'
      )!;
    await vi.waitFor(() => {
      flushSync();
      expect(publish()).toBeDefined();
      expect(publish().disabled).toBe(false);
    });
    publish().click();
    await vi.waitFor(() => {
      flushSync();
      expect(post).toHaveBeenCalledTimes(2);
      if (changed)
        expect(host.textContent).toContain(
          'This draft changed. Open it to review'
        );
      else expect(publish().disabled).toBe(true);
    });
    expect(post).toHaveBeenLastCalledWith(
      '/api/v3/route-drafts/{draft_id}/activate',
      expect.objectContaining({
        params: {
          path: { draft_id: receipt.id },
          header: {
            'If-Match': receipt.etag,
            'Idempotency-Key': expect.any(String)
          }
        }
      })
    );
    expect(get).not.toHaveBeenCalled();
    if (changed) {
      publish().click();
      await vi.waitFor(() => expect(post).toHaveBeenCalledTimes(3));
      expect(post).toHaveBeenNthCalledWith(
        3,
        '/api/v3/route-drafts/{draft_id}/activate',
        expect.objectContaining({
          params: expect.objectContaining({
            header: expect.objectContaining({ 'If-Match': receipt.etag })
          })
        })
      );
      expect(get).not.toHaveBeenCalled();
    }
  }
);
