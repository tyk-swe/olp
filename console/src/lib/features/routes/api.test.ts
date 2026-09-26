import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { simulateRouting } from '$lib/features/routes/api';

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

function captureSimulation() {
  authLifecycle.establishSession({
    user: {
      id: '01980000-0000-7000-8000-000000000401',
      email: 'operator@example.com',
      display_name: 'Operator',
      role: 'operator',
      access_scope: 'global'
    },
    csrf_token: 'csrf-routing-token'
  });
  return captureRequests(() => jsonResponse([]));
}

describe('simulateRouting', () => {
  it('sends configured controls with a synthetic user message for provider validation', async () => {
    const requests = captureSimulation();
    const tools = [{ name: 'weather', input_schema: { type: 'object' } }];
    const preferences = {
      allow_fallbacks: null,
      order: null,
      preferred_max_latency_ms: null,
      preferred_min_throughput: null,
      strategy: null,
      deny_data_collection: false,
      ignore: [],
      max_price: null,
      only: null,
      quantizations: null,
      regions: null,
      require_parameters: true,
      require_zero_data_retention: false
    };
    const responseFormat = {
      type: 'json_schema' as const,
      name: 'answer',
      strict: true,
      schema: { type: 'object' }
    };
    await simulateRouting({
      route: 'support',
      surface: 'anthropic',
      mode: 'unary',
      temperature: 0,
      max_output_tokens: 128,
      tools,
      response_format: responseFormat,
      preferences,
      apiKeyId: '01980000-0000-7000-8000-000000000402',
      seed: 'repeatable'
    });
    expect(new URL(requests[0].url).pathname).toBe('/api/v1/routing/simulate');
    expect(await requests[0].json()).toEqual({
      operation: {
        operation: 'generation',
        request: {
          route: 'support',
          messages: [
            { role: 'user', content: [{ type: 'text', text: 'Hello' }] }
          ],
          parameters: { stream: false, temperature: 0, max_output_tokens: 128 },
          tools,
          response_format: responseFormat
        }
      },
      surface: 'anthropic',
      mode: 'unary',
      preferences,
      api_key_id: '01980000-0000-7000-8000-000000000402',
      seed: 'repeatable'
    });
  });

  it('leaves unconfigured controls absent so they do not become requirements', async () => {
    const requests = captureSimulation();
    await simulateRouting({
      route: 'support',
      surface: 'openai',
      mode: 'unary'
    });
    expect((await requests[0].json()).operation.request).toEqual({
      route: 'support',
      messages: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }],
      parameters: { stream: false },
      tools: []
    });
  });
});
