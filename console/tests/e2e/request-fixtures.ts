import type { RequestAttempt, RequestDetail } from '../../src/lib/api/requests';
import type { Page } from '../playwright';

export const requestId = '01980000-0000-7000-8000-000000000101';
export const generationId = '01980000-0000-7000-8000-000000000102';
export const keyId = '01980000-0000-7000-8000-000000000103';
export const providerId = '01980000-0000-7000-8000-000000000104';
export const pricingRevisionId = '01980000-0000-7000-8000-000000000201';

const summary = {
  id: requestId,
  runtime_generation_id: generationId,
  api_key_id: keyId,
  route: 'support-chat',
  operation: 'generation',
  surface: 'openai',
  started_at: '2026-07-12T12:00:00Z',
  completed_at: '2026-07-12T12:00:00.245Z',
  status_code: 200,
  error_class: null,
  total_latency_ms: 245,
  first_byte_ms: 81,
  attempt_count: 1,
  input_tokens: 42,
  output_tokens: 18,
  cached_input_tokens: 0,
  estimated_cost: '0.00125',
  currency: null,
  unpriced: false,
  usage_complete: true
};

function attempt(
  ordinal: number,
  fields: Partial<RequestAttempt> = {}
): RequestAttempt {
  return {
    id: `01980000-0000-7000-8000-${String(104 + ordinal).padStart(12, '0')}`,
    ordinal,
    provider_id: providerId,
    provider_name: 'Primary OpenAI',
    upstream_model: 'gpt-test',
    started_at: summary.started_at,
    completed_at: summary.completed_at,
    status_code: 200,
    error_class: null,
    latency_ms: 245,
    first_byte_ms: 81,
    committed: true,
    charge_status: null,
    usage_observed: null,
    usage_complete: null,
    input_tokens: null,
    output_tokens: null,
    cached_input_tokens: null,
    media_units: null,
    estimated_cost: null,
    currency: null,
    unpriced: null,
    pricing_revision_id: null,
    ...fields
  };
}

export async function mockRequestExplorer(
  page: Page,
  detail: RequestDetail = {
    ...summary,
    attempts: [
      attempt(1, {
        charge_status: 'billable',
        usage_observed: true,
        usage_complete: summary.usage_complete,
        input_tokens: summary.input_tokens,
        output_tokens: summary.output_tokens,
        cached_input_tokens: summary.cached_input_tokens,
        estimated_cost: summary.estimated_cost,
        currency: summary.currency,
        unpriced: summary.unpriced
      })
    ]
  }
) {
  await page.route(
    /\/api\/v1\/requests(?:\/[^?]+)?(?:\?.*)?$/,
    async (route) => {
      const path = new URL(route.request().url()).pathname;
      await route.fulfill({
        json:
          path === `/api/v1/requests/${requestId}`
            ? detail
            : { items: [summary], next_cursor: null }
      });
    }
  );
}

export function twoChargeRequest(): RequestDetail {
  const billable = {
    charge_status: 'billable',
    usage_observed: true,
    usage_complete: true,
    input_tokens: 10,
    output_tokens: 5,
    currency: 'USD',
    unpriced: false,
    pricing_revision_id: pricingRevisionId
  };
  return {
    ...summary,
    attempt_count: 2,
    input_tokens: 20,
    output_tokens: 10,
    cached_input_tokens: 4,
    estimated_cost: '0.000062000000',
    currency: 'USD',
    attempts: [
      attempt(1, {
        ...billable,
        status_code: 503,
        error_class: 'upstream_http',
        committed: false,
        cached_input_tokens: 4,
        estimated_cost: '0.000042000000'
      }),
      attempt(2, {
        ...billable,
        provider_name: 'Fallback OpenAI',
        cached_input_tokens: 0,
        estimated_cost: '0.000020000000'
      })
    ]
  };
}

export function incompleteUsageRequest(): RequestDetail {
  return {
    ...summary,
    attempt_count: 6,
    input_tokens: 7,
    output_tokens: 0,
    cached_input_tokens: 0,
    estimated_cost: '0.000000000000',
    currency: 'USD',
    unpriced: true,
    usage_complete: false,
    attempts: [
      attempt(1),
      attempt(2, {
        charge_status: 'not_billable',
        usage_observed: false,
        usage_complete: true,
        unpriced: false
      }),
      attempt(3, {
        charge_status: 'billing_uncertain',
        usage_observed: false,
        usage_complete: false,
        unpriced: false,
        currency: 'USD',
        pricing_revision_id: pricingRevisionId
      }),
      attempt(4, {
        charge_status: 'billable',
        usage_observed: true,
        usage_complete: true,
        unpriced: true,
        media_units: '1.125000'
      }),
      attempt(5, {
        charge_status: 'billable',
        usage_observed: true,
        usage_complete: false,
        unpriced: false,
        input_tokens: 7,
        currency: 'USD',
        pricing_revision_id: pricingRevisionId
      }),
      attempt(6, {
        charge_status: 'billable',
        usage_observed: true,
        usage_complete: true,
        unpriced: false,
        input_tokens: 0,
        output_tokens: 0,
        cached_input_tokens: 0,
        estimated_cost: '0.000000000000',
        currency: 'USD',
        pricing_revision_id: pricingRevisionId
      })
    ]
  };
}
