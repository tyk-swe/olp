import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';

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
  const { data, error, response } = await apiClient.POST('/api/v1/playground', {
    cache: 'no-store',
    headers: { 'cache-control': 'no-store' },
    body: input
  });
  return result(data, error, response);
}
