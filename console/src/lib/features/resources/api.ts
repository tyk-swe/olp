import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  createHeaders,
  ensureOk,
  mutationHeaders,
  unwrap,
  unwrapPage,
  type CursorPage
} from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import { compactQuery } from '#lib/management/query.js';

export type ProviderResource = components['schemas']['ProviderResourceItem'];
export type RetrievalBinding =
  components['schemas']['RetrievalBindingResponse'];
export type RetrievalBindingInput =
  components['schemas']['RetrievalBindingInput'];
export type RetrievalBindingFields =
  components['schemas']['RetrievalBindingFields'];

export type ProviderResourceFilters = {
  cursor?: string;
  limit?: number;
  kind?: ProviderResource['kind'];
  api_key_id?: string;
  provider_id?: string;
  route?: string;
  state?: string;
};

const retainedEvidenceBodyPlaceholder: components['schemas']['RetainedUsageEvidenceRequest'] =
  {
    idempotency_key: '',
    evidence_digest: '',
    start: '',
    end: '',
    terminal: false
  };

export async function listProviderResources(
  filters: ProviderResourceFilters
): Promise<CursorPage<ProviderResource>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/provider-resources', {
      params: { query: compactQuery(filters) }
    })
  );
}

export async function getProviderResourceContracts(signal?: AbortSignal) {
  return unwrap(
    await apiClient.GET('/api/v1/provider-resource-contracts', { signal })
  );
}

export async function requestRetainedReconciliation(id: string, hint: string) {
  const trimmed = hint.trim();
  return unwrap(
    await apiClient.POST('/api/v1/provider-resources/{id}/reconcile', {
      params: { path: { id } },
      body: trimmed ? { upstream_id: trimmed } : {}
    })
  );
}

export async function submitRetainedEvidence(id: string, serialized: string) {
  return unwrap(
    await apiClient.POST('/api/v1/provider-resources/{id}/usage-evidence', {
      params: { path: { id } },
      body: retainedEvidenceBodyPlaceholder,
      bodySerializer: () => serialized
    })
  );
}

export const listRetrievalBindings = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/retrieval-bindings', {
        params: { query: { cursor, limit: 200 } }
      })
    )
  );

export const getRetrievalBinding = async (id: string) =>
  unwrap(
    await apiClient.GET('/api/v1/retrieval-bindings/{binding_id}', {
      params: { path: { binding_id: id } }
    })
  );

export const getRetrievalRevision = async (id: string, revision: string) =>
  unwrap(
    await apiClient.GET(
      '/api/v1/retrieval-bindings/{binding_id}/revisions/{revision_id}',
      {
        params: { path: { binding_id: id, revision_id: revision } }
      }
    )
  );

export async function saveRetrievalBinding(
  current: RetrievalBinding | null,
  input: RetrievalBindingInput,
  requestId: string
) {
  if (current) {
    return unwrap(
      await apiClient.PUT('/api/v1/retrieval-bindings/{binding_id}', {
        params: {
          path: { binding_id: current.id },
          header: mutationHeaders(current.etag, requestId)
        },
        body: input
      })
    );
  }
  return unwrap(
    await apiClient.POST('/api/v1/retrieval-bindings', {
      params: { header: createHeaders(requestId) },
      body: input
    })
  );
}

export async function retireRetrievalBinding(
  current: RetrievalBinding,
  requestId: string
) {
  ensureOk(
    await apiClient.DELETE('/api/v1/retrieval-bindings/{binding_id}', {
      params: {
        path: { binding_id: current.id },
        header: mutationHeaders(current.etag, requestId)
      }
    })
  );
}
