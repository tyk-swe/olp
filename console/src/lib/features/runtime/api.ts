import type { components } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import { unwrapPage } from '$lib/management/http';
import { RUNTIME_GENERATION_PAGE_SIZE } from '$lib/management/pageSizes';
import type { CursorPage } from '$lib/management/http';
import { compactQuery } from '$lib/management/query';

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
  return unwrapPage({ data, error, response });
}
