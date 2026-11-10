import { QueryClient } from '@tanstack/svelte-query';
import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { json, serve } from '#lib/management/test/transport.js';
import CodeDiagnosticsProbe from './test/CodeDiagnosticsProbe.svelte';

function attempt(
  id: string,
  decision: Record<string, unknown> | null = {
    attempt_id: id,
    chosen_account_id: `chosen-${id}`,
    reason: 'chosen',
    skipped: []
  }
) {
  return {
    id,
    account_id: 'account',
    operation: 'responses',
    model: 'fixture-model',
    state: 'settled',
    route_id: 'route',
    route_revision_id: 'revision',
    api_key_id: 'key',
    created_at: '2026-01-01T00:00:00Z',
    decision
  };
}

function render() {
  const target = document.createElement('div');
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(CodeDiagnosticsProbe, {
    target,
    props: { client, projectId: 'project' }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
  });
  flushSync();
  return target;
}

afterEach(() => vi.unstubAllGlobals());

it('renders inline attempt decisions across pages without issuing per-attempt decision queries', async () => {
  const decisionQueries: string[] = [];
  serve((request) => {
    const url = new URL(request.url);
    if (url.pathname === '/api/v1/code/attempts') {
      return url.searchParams.has('cursor')
        ? json({ items: [attempt('third')], next_cursor: null })
        : json({
            items: [attempt('first'), attempt('second', null)],
            next_cursor: 'next'
          });
    }
    if (url.pathname === '/api/v1/code/accounts') return json({ items: [] });
    if (url.pathname === '/api/v1/code/decisions') {
      decisionQueries.push(url.search);
      return json({ title: 'Unexpected N+1 decision query', status: 500 }, 500);
    }
    return json({ title: 'Unexpected request', status: 404 }, 404);
  });
  const target = render();
  await vi.waitFor(() => {
    expect(target.textContent).toContain('chosen-first');
    expect(target.textContent).toContain(
      'No decision recorded for this attempt'
    );
  });
  const next = [...target.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === 'Next'
  );
  expect(next).toBeDefined();
  next?.click();
  await vi.waitFor(() => expect(target.textContent).toContain('chosen-third'));
  expect(decisionQueries).toEqual([]);
});
