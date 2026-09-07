import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import { RUNTIME_GENERATION_PAGE_SIZE } from '$lib/api/pageSizes';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type RuntimeGeneration = components['schemas']['RuntimeGenerationItem'];

export async function listRuntimeGenerations(
  cursor?: string
): Promise<CursorPage<RuntimeGeneration>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/runtime-generations',
    {
      params: {
        query: compactQuery({ cursor, limit: RUNTIME_GENERATION_PAGE_SIZE })
      }
    }
  );
  return pageResult(result(data, error, response));
}
