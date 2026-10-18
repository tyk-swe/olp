import { afterEach, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

it.each([
  ['openai-decisions', '/v1/decisions'],
  ['systemone', '/v1/systemone']
])(
  'sends %s through its public endpoint for transformed routes',
  async (dialect, path) => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('{"answers":[]}', {
        headers: { 'Content-Type': 'application/json' }
      })
    );
    await runNativeOperation(
      'decision',
      dialect,
      'decision-route',
      'olp_test',
      '{"model":"","questions":[]}'
    );
    expect(gatewayFetch).toHaveBeenCalledWith(
      path,
      expect.objectContaining({
        method: 'POST',
        body: '{"model":"decision-route","questions":[]}'
      })
    );
  }
);
