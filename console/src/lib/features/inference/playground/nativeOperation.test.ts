import { afterEach, expect, it, vi } from 'vitest';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

it.each([
  'perplexity-search',
  'tavily-search',
  'exa-search',
  'parallel-search'
])('refuses a body model for %s', async (dialect) => {
  await expect(
    runNativeOperation(
      'search',
      dialect,
      'search-route',
      'olp_test',
      '{"model":"search-route","query":"q"}'
    )
  ).rejects.toThrow('no model field');
});
