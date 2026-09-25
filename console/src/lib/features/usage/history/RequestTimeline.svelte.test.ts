import { flushSync, mount, unmount } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CreateQueryResult } from '@tanstack/svelte-query';
import RequestTimeline from './RequestTimeline.svelte';
import type {
  RequestAttempt,
  RequestDetail
} from '$lib/features/usage/history/api';

function attempt(overrides: Partial<RequestAttempt>): RequestAttempt {
  return {
    id: '11111111-1111-4111-8111-111111111111',
    ordinal: 1,
    committed: true,
    provider_id: '22222222-2222-4222-8222-222222222222',
    provider_name: 'Provider',
    upstream_model: 'vendor-model',
    started_at: '2026-01-01T00:00:00Z',
    ...overrides
  };
}

function detailWith(attempts: RequestAttempt[]): RequestDetail {
  return {
    id: '33333333-3333-4333-8333-333333333333',
    route: 'route',
    api_key_id: '44444444-4444-4444-8444-444444444444',
    runtime_generation_id: '55555555-5555-4555-8555-555555555555',
    attempt_count: attempts.length,
    attempts,
    operation: 'generation',
    surface: 'openai',
    started_at: '2026-01-01T00:00:00Z',
    attribution: {},
    policy_decisions: [],
    status_code: 200
  };
}

function mountTimeline(detail: RequestDetail) {
  const host = document.createElement('div');
  document.body.append(host);
  const component = mount(RequestTimeline, {
    target: host,
    props: {
      detail: {
        isPending: false,
        isError: false,
        error: null,
        data: detail,
        refetch: vi.fn()
      } as unknown as CreateQueryResult<RequestDetail>
    }
  });
  flushSync();
  return { host, component };
}

describe('request timeline attempt evidence', () => {
  const mounted: { host: HTMLElement; component?: ReturnType<typeof mount> }[] =
    [];

  afterEach(async () => {
    while (mounted.length) {
      const entry = mounted.pop()!;
      if (entry.component) await unmount(entry.component);
      entry.host.remove();
    }
  });

  it('renders recorded interaction and outcome facts beside the HTTP status', () => {
    const entry = mountTimeline(
      detailWith([
        attempt({
          status_code: 200,
          routing: {
            provider_revision_id: '66666666-6666-4666-8666-666666666666',
            interaction: {
              fidelity: 'strict',
              plan_class: 'qualified_interaction',
              upstream_state: 'terminal',
              client_state: 'actionable'
            },
            outcome: {
              native_status: 'completed',
              fault_origin: null,
              fault_scope: null,
              fault_resource: null,
              limit_category: null,
              limit: null
            }
          }
        })
      ])
    );
    mounted.push(entry);
    const text = entry.host.textContent ?? '';
    expect(text).toContain('Fidelity');
    expect(text).toContain('strict');
    expect(text).toContain('qualified interaction');
    expect(text).toContain('terminal');
    expect(text).toContain('Native status');
    expect(text).toContain('completed');
    expect(text).not.toContain('Outcome facts were not recorded');
  });

  it('renders a recorded fault and exhausted limit without HTTP inference', () => {
    const entry = mountTimeline(
      detailWith([
        attempt({
          status_code: 200,
          routing: {
            provider_revision_id: '66666666-6666-4666-8666-666666666666',
            outcome: {
              native_status: null,
              fault_origin: 'proxy_capacity',
              fault_scope: 'endpoint',
              fault_resource: 'event_bytes',
              limit_category: 'bytes',
              limit: 1048576
            }
          }
        })
      ])
    );
    mounted.push(entry);
    const text = entry.host.textContent ?? '';
    expect(text).toContain('proxy capacity');
    expect(text).toContain('event_bytes');
    expect(text).toContain('bytes');
    expect(text).toContain('1,048,576');
    expect(text).toContain('Strict interaction evidence was not recorded');
  });

  it('marks outcome facts unavailable on records that predate them', () => {
    const entry = mountTimeline(
      detailWith([
        attempt({
          status_code: 200,
          routing: {
            provider_revision_id: '66666666-6666-4666-8666-666666666666'
          }
        })
      ])
    );
    mounted.push(entry);
    const text = entry.host.textContent ?? '';
    expect(text).toContain('Outcome facts were not recorded for this attempt');
    expect(text).toContain('Strict interaction evidence was not recorded');
    expect(text).not.toContain('Native status');
  });
});
