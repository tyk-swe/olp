// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  exportRequestsCsv,
  type RequestSummary
} from '$lib/features/usage/history/api';
import { requestFilters } from '$lib/features/usage/history/requestListState';
import RequestResultsProbe from './test/RequestResultsProbe.svelte';

vi.mock('$lib/download', () => ({ downloadBlob: vi.fn() }));
vi.mock('$lib/features/usage/history/api', async (original) => ({
  ...(await original<typeof import('$lib/features/usage/history/api')>()),
  exportRequestsCsv: vi.fn()
}));
vi.mock('$app/state', () => ({
  page: { url: new URL('https://console.test/requests?session_id=URLSession') }
}));

const item: RequestSummary = {
  id: '01980000-0000-7000-8000-000000000001',
  route: 'primary',
  model: 'model/one',
  operation: 'chat',
  surface: 'openai',
  provider_name: 'upstream',
  api_key_id: '01980000-0000-7000-8000-000000000103',
  project_id: '01980000-0000-7000-8000-000000000901',
  status_code: 200,
  error_class: null,
  attempt_count: 1,
  started_at: '2026-10-08T12:00:00Z',
  completed_at: '2026-10-08T12:00:01Z',
  first_byte_ms: 100,
  total_latency_ms: 500,
  input_tokens: 10,
  output_tokens: 5,
  cached_input_tokens: 0,
  cache_write_input_tokens: 0,
  estimated_cost: null,
  currency: null,
  usage_complete: true,
  unpriced: false,
  attribution: {},
  policy_decisions: [],
  payload_captured: false
} as unknown as RequestSummary;

function makeState() {
  return {
    applied: requestFilters({
      route: '',
      providerId: '',
      model: '',
      apiKeyId: '',
      projectId: '',
      sessionId: 'SessionCase',
      attributionKey: '',
      attributionValue: '',
      operation: '',
      statusCode: '',
      errorClass: '',
      startedAfter: '2026-07-12T13:30:42.123456Z',
      startedBefore: '2026-07-12T22:00:12.456789Z'
    }),
    cursor: undefined,
    history: [] as string[],
    route: '',
    providerId: '',
    model: '',
    apiKeyId: '',
    projectId: '',
    sessionId: 'DraftSession',
    attributionKey: '',
    attributionValue: '',
    operation: '',
    statusCode: '',
    errorClass: '',
    startedAfter: '',
    startedBefore: ''
  };
}

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function render(
  problem: string | null = null,
  queryProblem: string | null = null
) {
  const state = makeState();
  component = mount(RequestResultsProbe, {
    target: host,
    props: {
      client,
      items: [item],
      listState: state,
      applyFilters: vi.fn(),
      resetFilters: vi.fn(),
      problem,
      queryProblem
    }
  });
  flushSync();
  return state;
}

it('exports the applied filters — not the draft or URL — as CSV', async () => {
  const { downloadBlob } = await import('$lib/download');
  vi.mocked(exportRequestsCsv).mockResolvedValue('request_id\nabc\n');
  render();

  const button = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent === 'Export CSV'
  )!;
  button.click();
  await vi.waitFor(() => expect(exportRequestsCsv).toHaveBeenCalled());

  expect(exportRequestsCsv).toHaveBeenCalledWith(
    expect.objectContaining({
      session_id: 'SessionCase',
      started_after: '2026-07-12T13:30:42.123456Z',
      started_before: '2026-07-12T22:00:12.456789Z'
    })
  );
  expect(vi.mocked(downloadBlob)).toHaveBeenCalled();
});

it('hides the CSV control when the query itself is invalid', () => {
  render(null, 'Started before must be later than started after.');
  expect(
    [...host.querySelectorAll('button')].find(
      (candidate) => candidate.textContent === 'Export CSV'
    )
  ).toBeUndefined();
});

it('surfaces a download failure as an alert', async () => {
  const { downloadBlob } = await import('$lib/download');
  vi.mocked(exportRequestsCsv).mockResolvedValue('request_id\nabc\n');
  vi.mocked(downloadBlob).mockImplementation(() => {
    throw new Error('Blocked');
  });
  render();

  const button = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent === 'Export CSV'
  )!;
  button.click();
  await vi.waitFor(() =>
    expect(host.querySelector('[role="alert"]')).not.toBeNull()
  );
});

it('orders the session timeline by exact instant, oldest first', async () => {
  const state = makeState();
  const earlier = {
    ...item,
    id: '01980000-0000-7000-8000-000000000002',
    started_at: '2026-10-08T12:00:00.000000002Z'
  };
  const later = {
    ...item,
    id: '01980000-0000-7000-8000-000000000001',
    started_at: '2026-10-08T12:00:00.000000001Z'
  };
  component = mount(RequestResultsProbe, {
    target: host,
    props: {
      client,
      items: [earlier, later],
      listState: state,
      applyFilters: vi.fn(),
      resetFilters: vi.fn(),
      problem: null,
      queryProblem: null
    }
  });
  flushSync();
  await vi.waitFor(() =>
    expect(host.querySelector('.session-card')).not.toBeNull()
  );

  const entries = [...host.querySelectorAll('.session-list li')];
  expect(entries).toHaveLength(2);
  expect(entries[0]!.textContent).toContain('primary');
  expect(host.textContent).toContain('SessionCase');
  expect(host.textContent).toContain('Current page, oldest first');
  const datetimes = [
    ...host.querySelectorAll<HTMLTimeElement>('.session-list time')
  ].map((el) => el.getAttribute('datetime'));
  expect(datetimes).toEqual([
    '2026-10-08T12:00:00.000000001Z',
    '2026-10-08T12:00:00.000000002Z'
  ]);
  const link = host.querySelector<HTMLAnchorElement>('.session-list a')!;
  expect(link.href).toContain(`/requests/${later.id}`);
  expect(link.search).toContain('session_id=URLSession');
});

it('hides the session timeline without an applied session filter', () => {
  const state = makeState();
  state.applied = {};
  component = mount(RequestResultsProbe, {
    target: host,
    props: {
      client,
      items: [item],
      listState: state,
      applyFilters: vi.fn(),
      resetFilters: vi.fn(),
      problem: null,
      queryProblem: null
    }
  });
  flushSync();
  expect(host.querySelector('.session-card')).toBeNull();
});

it.each(['pending', 'error', 'placeholder'] as const)(
  'hides the session timeline while the request query is %s',
  async (state) => {
    const listState = makeState();
    component = mount(RequestResultsProbe, {
      target: host,
      props: {
        client,
        items: [item],
        state,
        listState,
        applyFilters: vi.fn(),
        resetFilters: vi.fn(),
        problem: null,
        queryProblem: null
      }
    });
    flushSync();
    await vi.waitFor(() =>
      expect(host.querySelector('.session-card')).toBeNull()
    );
  }
);
