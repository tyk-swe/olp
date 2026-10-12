import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, it } from 'vitest';
import RequestTimeline from './RequestTimeline.svelte';
import type { RequestDetail } from './api';
import type { CreateQueryResult } from '@tanstack/svelte-query';

let host: HTMLElement | undefined;
let component: ReturnType<typeof mount> | undefined;

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  host?.remove();
  host = undefined;
});

const detail = {
  id: 'request-1',
  route_slug: 'team-chat',
  started_at: '2026-09-15T12:00:00Z',
  completed_at: '2026-09-15T12:00:01Z',
  status_code: 200,
  committed: true,
  committed_at: '2026-09-15T12:00:01Z',
  latency_ms: 1000,
  budget_boundary: null,
  usage_observed: true,
  usage_complete: true,
  unpriced: false,
  input_tokens: 100,
  cached_input_tokens: 10,
  cache_write_input_tokens: 0,
  cache_write_5m_input_tokens: 0,
  cache_write_1h_input_tokens: 0,
  output_tokens: 50,
  media_units: null,
  estimated_cost: '0',
  currency: 'USD',
  pricing_revision_id: 'rev-1',
  attempt_count: 1,
  attempt_summary: null,
  policy_outcomes: [],
  policy_decisions: [],
  provider_id: 'provider-1',
  upstream_model: 'audio-model',
  attempts: [
    {
      id: 'attempt-1',
      ordinal: 1,
      provider_id: 'provider-1',
      provider_name: 'provider-one',
      upstream_model: 'audio-model',
      started_at: '2026-09-15T12:00:00Z',
      completed_at: '2026-09-15T12:00:01Z',
      committed: true,
      status_code: 200,
      error_class: null,
      first_byte_ms: null,
      latency_ms: 1000,
      routing: null,
      charge_status: 'billable',
      usage_observed: true,
      usage_complete: true,
      unpriced: false,
      input_tokens: 100,
      cached_input_tokens: 10,
      cache_write_input_tokens: 0,
      cache_write_5m_input_tokens: 0,
      cache_write_1h_input_tokens: 0,
      output_tokens: 50,
      media_units: null,
      estimated_cost: '0',
      currency: 'USD',
      pricing_revision_id: 'rev-1',
      audio_input_tokens: 40,
      cached_audio_input_tokens: 10,
      audio_output_tokens: 20,
      image_input_tokens: 5,
      cached_image_input_tokens: null,
      image_output_tokens: null,
      tool_calls: { web_search: 2 },
      batch: true,
      pricing_components: ['audio_input_per_million', 'commitment'],
      commitment_weight: '0.005000000000',
      commitment_amount: '300.000000000000',
      commitment_period: 'day'
    }
  ]
} as unknown as RequestDetail;

it('shows attempt modality tokens, tools, batch, components and commitment', () => {
  host = document.createElement('div');
  document.body.append(host);
  component = mount(RequestTimeline, {
    target: host,
    props: {
      detail: {
        isPending: false,
        isError: false,
        data: detail,
        refetch: () => {}
      } as unknown as CreateQueryResult<RequestDetail>
    }
  });
  flushSync();
  const text = host.textContent ?? '';
  expect(text).toContain('Audio tokens');
  expect(text).toContain('40 in · 20 out');
  expect(text).toContain('Image tokens');
  expect(text).toContain('5 in · — out');
  expect(text).toContain('web_search × 2');
  expect(text).toContain('Batch pricing');
  expect(text).toContain('audio_input_per_million, commitment');
  expect(text).toContain('Commitment');
  expect(text).toContain('$300.00');
  expect(text).toContain('0.005000000000');
});
