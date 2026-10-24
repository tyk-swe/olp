import { apiClient } from '$lib/api/client';
import type { components } from '$lib/api/schema';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

export type RetrievalBinding =
  components['schemas']['RetrievalBindingResponse'];
export type RetrievalBindingInput =
  components['schemas']['RetrievalBindingInput'];

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
  return current
    ? unwrap(
        await apiClient.PUT('/api/v1/retrieval-bindings/{binding_id}', {
          params: {
            path: { binding_id: current.id },
            header: { 'If-Match': current.etag, 'Idempotency-Key': requestId }
          },
          body: input
        })
      )
    : unwrap(
        await apiClient.POST('/api/v1/retrieval-bindings', {
          params: { header: { 'Idempotency-Key': requestId } },
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
        header: { 'If-Match': current.etag, 'Idempotency-Key': requestId }
      }
    })
  );
}
