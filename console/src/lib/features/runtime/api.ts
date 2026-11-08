import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrapPage } from '#lib/management/http.js';
import { RUNTIME_GENERATION_PAGE_SIZE } from '#lib/management/pageSizes.js';
import type { CursorPage } from '#lib/management/http.js';
import { compactQuery } from '#lib/management/query.js';

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
