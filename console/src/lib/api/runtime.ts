import type { components } from './schema';
import { apiClient } from './client';
import { pageResult, result } from './http';
import { RUNTIME_GENERATION_PAGE_SIZE } from './pageSizes';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from './query';

export type RuntimeGeneration = components['schemas']['RuntimeGenerationItem'];

export async function listRuntimeGenerations(
  cursor?: string
): Promise<CursorPage<RuntimeGeneration>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/runtime-generations',
    {
      params: {
        query: compactQuery({ cursor, limit: RUNTIME_GENERATION_PAGE_SIZE })
      }
    }
  );
  return pageResult(result(data, error, response));
}
