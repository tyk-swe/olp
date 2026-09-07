import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

export type PlaygroundRequest = Omit<
  components['schemas']['PlaygroundRequest'],
  'surface'
> & {
  surface?: 'openai' | 'anthropic' | 'gemini';
};
export type PlaygroundResponse = components['schemas']['PlaygroundResponse'];

export async function runPlayground(
  input: PlaygroundRequest
): Promise<PlaygroundResponse> {
  const { data, error, response } = await apiClient.POST('/api/v3/playground', {
    cache: 'no-store',
    headers: { 'cache-control': 'no-store' },
    body: input
  });
  return result(data, error, response);
}
