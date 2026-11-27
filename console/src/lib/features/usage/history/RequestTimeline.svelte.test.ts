import { createQuery } from '@tanstack/svelte-query';
import { getAllContexts, mount, unmount } from 'svelte';
import { expect, it, onTestFinished } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountState } from '#test/mount.js';
import { getRequest } from './api.js';
import RequestTimeline from './RequestTimeline.svelte';

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
    const { target, value: component } = mountState(({ target }) => {
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
    onTestFinished(() => unmount(component));
    await eventually(() =>
      expect(target.querySelector('p.muted')?.textContent?.trim()).toBe(
        `Response cache: ${result}${suffix}`
      )
    );
  }
);
