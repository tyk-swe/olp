import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import { type CursorPage, toCursorPage } from './pagination';
import { compactQuery } from './query';

export type RuntimeGeneration = components['schemas']['RuntimeGenerationItem'];

export async function listRuntimeGenerations(
  cursor?: string
): Promise<CursorPage<RuntimeGeneration>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/runtime-generations',
    { params: { query: compactQuery({ cursor, limit: 25 }) } }
  );
  return toCursorPage(result(data, error, response));
}
