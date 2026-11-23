import { createQuery } from '@tanstack/svelte-query';
import { getAllContexts, mount, unmount } from 'svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import { getRequest } from './api.js';
import RequestTimeline from './RequestTimeline.svelte';

afterEach(() => vi.unstubAllGlobals());

it.each([
  ['bypass', 'no_store', ' · Caller opted out (no-store)'],
  ['miss', 'refresh', ' · Caller requested a refresh (no-cache)'],
  ['bypass', 'multiple_choices', ' · More than one response choice'],
  ['bypass', 'backend_unavailable', ' · Cache backend unavailable'],
  ['bypass', undefined, ' · Reason not recorded'],
  ['miss', undefined, ''],
  ['hit', undefined, '']
])(
  'shows the %s result with reason %s only when appropriate',
  async (result, reason, suffix) => {
    serve(
      () =>
        json({
          cache_result: result,
          cache_lookup_reason: reason,
          attempts: []
        }),
      ['read', 'usage']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() => {
      const detail = createQuery(() => ({
        queryKey: ['request', result, reason],
        queryFn: () => getRequest('request-id')
      }));
      return mount(RequestTimeline, {
        target,
        props: { detail },
        context: getAllContexts()
      });
    });
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    await vi.waitFor(() =>
      expect(target.querySelector('p.muted')?.textContent?.trim()).toBe(
        `Response cache: ${result}${suffix}`
      )
    );
  }
);
