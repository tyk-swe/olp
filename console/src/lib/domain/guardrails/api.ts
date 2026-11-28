import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrapPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type Definition = components['schemas']['GuardrailResponse'];

export const listDefinitions = (signal?: AbortSignal): Promise<Definition[]> =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/guardrails', {
        params: { query: { cursor } },
        signal
      })
    )
  );
