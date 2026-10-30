// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import type { CodeAccount, CodeAttempt } from '$lib/api/code-mode';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import CodeDiagnosticsProbe from './test/CodeDiagnosticsProbe.svelte';

const projectId = '01980000-0000-7000-8000-000000000001';
const accountId = '01980000-0000-7000-8000-000000000010';
const firstId = '01980000-0000-7000-8000-000000000020';
const recoveredId = '01980000-0000-7000-8000-000000000021';
const bindingId = '01980000-0000-7000-8000-000000000030';
const revisionId = '01980000-0000-7000-8000-000000000040';

const attempt: CodeAttempt = {
  id: firstId,
  project_id: projectId,
  route_id: projectId,
  route_revision_id: revisionId,
  api_key_id: projectId,
  binding_id: bindingId,
  account_id: accountId,
  operation: 'responses',
  requested_model: 'team-code',
  model: 'fixture-model',
  state: 'settled',
  reserved_tokens: 0,
  reported_tokens: 0,
  input_tokens: null,
  output_tokens: null,
  cached_tokens: null,
  reasoning_tokens: null,
  bound_evidence: null,
  refusal: null,
  created_at: '2026-01-01T00:00:00Z',
  finished_at: '2026-01-01T00:00:01Z',
  outcome_origin: 'upstream',
  outcome: 'rejected',
  upstream_status: 429,
  outcome_observed_at: '2026-01-01T00:00:01Z',
  end_user_digest: '',
  initial_recovery: true,
  nonexecution: true,
  recovery_from: null,
  recovery_reason: null
};
const account: CodeAccount = {
  id: accountId,
  project_id: projectId,
  provider_id: accountId,
  credential_id: accountId,
  principal: 'fixture-principal',
  name: 'Fixture account',
  adapter: 'codex',
  enabled: true,
  eligible: false,
  models: ['fixture-model'],
  health: 'quota_limited',
  grant_state: 'current',
  allowance: null,
  etag: projectId
};

function render(
  items: CodeAttempt[],
  accountsAvailable = true,
  sourceAttempts: CodeAttempt[] = []
) {
  authLifecycle.establishSession({
    user: {
      id: accountId,
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: []
    },
    csrf_token: 'fixture'
  });
  const fetch = vi.fn(async (request: Request) => {
    const url = new URL(request.url);
    if (url.pathname.endsWith('/accounts')) {
      return accountsAvailable
        ? Response.json(
            url.searchParams.has('cursor')
              ? { items: [account], next_cursor: null }
              : { items: [], next_cursor: 'next-account-page' }
          )
        : Response.json({ title: 'Unavailable', status: 503 }, { status: 503 });
    }
    return Response.json({
      items: url.searchParams.has('attempt_id')
        ? [...items, ...sourceAttempts].filter(
            (item) => item.id === url.searchParams.get('attempt_id')
          )
        : items,
      next_cursor: null
    });
  });
  vi.stubGlobal('fetch', fetch);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(CodeDiagnosticsProbe, {
    target: host,
    props: { client, projectId }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.unstubAllGlobals();
  });
  flushSync();
  return { host, fetch };
}

it('links safe recovery to the rejected attempt and separates current eligibility from recorded evidence', async () => {
  const recovered: CodeAttempt = {
    ...attempt,
    id: recoveredId,
    model: 'fixture-alternate',
    upstream_status: 200,
    outcome: 'completed',
    reported_tokens: 12,
    nonexecution: false,
    recovery_from: firstId,
    recovery_reason: 'quota_rejected'
  };
  const { host, fetch } = render([recovered, attempt]);
  await vi.waitFor(() => expect(host.textContent).toContain('Quota rejection'));
  const cards = host.querySelectorAll('article');
  expect(cards[0].textContent).toContain(firstId);
  expect(cards[0].textContent).toContain(revisionId);
  expect(cards[0].textContent).toContain('team-code');
  expect(cards[0].textContent).toContain('fixture-alternate');
  expect(cards[0].textContent).toContain('Reported total');
  expect(cards[1].textContent).toContain('Proven not executed');
  expect(cards[1].textContent).toContain('Confirmed zero consumption');
  await vi.waitFor(() =>
    expect(cards[0].textContent).toContain('Currently ineligible')
  );
  expect(host.textContent).toContain(
    'Admission-time eligibility was not retained'
  );
  [...cards[0].querySelectorAll<HTMLButtonElement>('button')]
    .find((button) => button.textContent === 'Show conversation attempts')!
    .click();
  await vi.waitFor(() =>
    expect(
      fetch.mock.calls
        .map(([request]) => new URL(request.url))
        .some(
          (url) =>
            url.pathname.endsWith('/attempts') &&
            url.searchParams.get('binding_id') === bindingId &&
            url.searchParams.get('project_id') === projectId
        )
    ).toBe(true)
  );
});

it('explains when a recorded recovery source is unavailable under current access', async () => {
  const { host } = render([
    {
      ...attempt,
      id: recoveredId,
      recovery_from: firstId,
      recovery_reason: null,
      nonexecution: false
    }
  ]);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Recovery reason not recorded')
  );
  [...host.querySelectorAll<HTMLButtonElement>('article button')]
    .find((button) => button.textContent === firstId)!
    .click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'The source may be unavailable under your current project access'
    )
  );
  expect(host.querySelector('article')).toBeNull();
});

it.each([
  { outcome: 'incomplete', upstream_status: 200 },
  { outcome: 'rejected', upstream_status: 429 },
  { outcome: 'transport_error', upstream_status: null }
] as const)(
  'keeps $outcome consumption unknown without inventing nonexecution or recovery',
  async ({ outcome, upstream_status }) => {
    const { host } = render([
      {
        ...attempt,
        state: 'uncertain',
        outcome,
        upstream_status,
        reported_tokens: null,
        output_tokens: 2,
        nonexecution: false,
        initial_recovery: false
      }
    ]);
    await vi.waitFor(() =>
      expect(host.querySelector('article')?.textContent).toContain(
        'Unknown consumption'
      )
    );
    const card = host.querySelector('article')!;
    expect(card.textContent).toContain('Nonexecution not proven');
    expect(card.textContent).toContain('Portable initial input not recorded');
    expect(card.textContent).toContain('No recovery source recorded');
    expect(card.textContent).not.toContain('Confirmed zero consumption');
    expect(card.textContent).not.toContain('Quota rejection');
    expect(card.textContent).toContain('does not retry or replay');
  }
);

it('retains credential-recovery diagnostics when current account lookup fails', async () => {
  const { host } = render(
    [
      {
        ...attempt,
        id: recoveredId,
        upstream_status: 200,
        outcome: 'completed',
        reported_tokens: 7,
        recovery_from: firstId,
        recovery_reason: 'credential_rejected',
        nonexecution: false
      }
    ],
    false
  );
  await vi.waitFor(() =>
    expect(host.querySelector('article')?.textContent).toContain(
      'Credential rejection, proven not executed'
    )
  );
  const card = host.querySelector('article')!;
  expect(card.textContent).toContain('Reported total');
  expect(card.textContent).toContain(firstId);
  expect(card.textContent).not.toContain('Currently eligible');
  expect(card.textContent).not.toContain('Currently ineligible');
});

it('follows the paid binding without looking up subscription eligibility', async () => {
  const paidBinding = '01980000-0000-7000-8000-000000000050';
  const { host, fetch } = render(
    [
      {
        ...attempt,
        id: recoveredId,
        account_id: null,
        binding_id: null,
        paid_binding_id: paidBinding,
        paid_provider_id: projectId,
        paid_slot_id: accountId,
        paid_monetary_state: 'unknown',
        initial_recovery: false,
        nonexecution: false,
        recovery_from: firstId,
        recovery_reason: 'quota_rejected'
      }
    ],
    true,
    [attempt]
  );
  await vi.waitFor(() =>
    expect(host.querySelector('article')?.textContent).toContain(
      'Paid target, subscription eligibility does not apply'
    )
  );
  const conversation = [
    ...host.querySelectorAll<HTMLButtonElement>('article button')
  ].find((button) => button.textContent === 'Show conversation attempts')!;
  conversation.click();
  await vi.waitFor(() =>
    expect(
      fetch.mock.calls.some(([request]) => {
        const url = new URL(request.url);
        return (
          url.searchParams.get('binding_id') === paidBinding &&
          url.searchParams.get('project_id') === projectId
        );
      })
    ).toBe(true)
  );
  expect(
    fetch.mock.calls.some(([request]) => request.url.includes('/accounts'))
  ).toBe(false);
  expect(host.querySelector('article')?.textContent).toContain(
    'Paid accounting'
  );
  expect(host.querySelector('article')?.textContent).toContain('unknown');
  const source = [
    ...host.querySelectorAll<HTMLButtonElement>('article button')
  ].find((button) => button.textContent === firstId)!;
  source.click();
  await vi.waitFor(() =>
    expect(host.querySelector('article')?.textContent).toContain(
      'Proven not executed'
    )
  );
  expect(host.querySelector('article')?.textContent).toContain(firstId);
  expect(host.querySelector('article')?.textContent).toContain(accountId);
  expect(
    fetch.mock.calls.some(([request]) => {
      const url = new URL(request.url);
      return (
        url.searchParams.get('attempt_id') === firstId &&
        url.searchParams.get('project_id') === projectId &&
        !url.searchParams.has('binding_id')
      );
    })
  ).toBe(true);
});
