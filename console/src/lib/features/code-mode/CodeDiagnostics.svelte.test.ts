import { expect, it } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState } from '#test/mount.js';
import CodeDiagnostics from './CodeDiagnostics.svelte';

function attempt(id: string) {
  return {
    id,
    account_id: 'account',
    operation: 'responses',
    model: 'fixture-model',
    state: 'settled',
    route_id: 'route',
    route_revision_id: 'revision',
    api_key_id: 'key',
    created_at: '2026-01-01T00:00:00Z'
  };
}

function decision(id: string) {
  return {
    attempt_id: id,
    chosen_account_id: `chosen-${id}`,
    reason: 'chosen',
    skipped: []
  };
}

function render() {
  return mountWithState(
    CodeDiagnostics,
    { projectId: 'project', kind: 'attempts', allowed: false },
    {
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { retry: false } })
    }
  ).target;
}

it('loads decisions only for attempts on the displayed page', async () => {
  const requested: string[] = [];
  serve((request) => {
    const url = new URL(request.url);
    if (url.pathname === '/api/v1/code/attempts') {
      return url.searchParams.has('cursor')
        ? json({ items: [attempt('third')], next_cursor: null })
        : json({
            items: [attempt('first'), attempt('second')],
            next_cursor: 'next'
          });
    }
    if (url.pathname === '/api/v1/code/accounts') return json({ items: [] });
    if (url.pathname === '/api/v1/code/decisions') {
      const id = url.searchParams.get('attempt_id') ?? '';
      requested.push(id);
      return json({ items: id === 'second' ? [] : [decision(id)] });
    }
    return json({ title: 'Unexpected request', status: 404 }, 404);
  });
  const target = render();
  await eventually(() => {
    expect(target.textContent).toContain('chosen-first');
    expect(target.textContent).toContain(
      'No decision recorded for this attempt'
    );
  });
  expect(requested.toSorted()).toEqual(['first', 'second']);
  const next = findButton(target, 'Next');
  expect(next).toBeDefined();
  next?.click();
  await eventually(() => expect(target.textContent).toContain('chosen-third'));
  expect(requested.toSorted()).toEqual(['first', 'second', 'third']);
});

it('shows lookup errors without hiding successful decisions', async () => {
  serve((request) => {
    const url = new URL(request.url);
    if (url.pathname === '/api/v1/code/attempts')
      return json({ items: [attempt('first'), attempt('second')] });
    if (url.pathname === '/api/v1/code/accounts') return json({ items: [] });
    if (url.pathname === '/api/v1/code/decisions') {
      const id = url.searchParams.get('attempt_id');
      return id === 'second'
        ? json({ items: [decision(id)] })
        : json({ title: 'Decision lookup failed', status: 503 }, 503);
    }
    return json({ title: 'Unexpected request', status: 404 }, 404);
  });
  const target = render();
  await eventually(() => {
    expect(target.textContent).toContain('Decision lookup failed');
    expect(target.textContent).toContain('chosen-second');
  });
  expect(target.textContent).not.toContain(
    'No decision recorded for this attempt'
  );
});
