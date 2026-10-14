import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap } from '$lib/api/http';

type Schemas = components['schemas'];

export type BillingImport = Schemas['BillingImport'];
export type PutBillingImportInput = Schemas['PutBillingImportRequest'];

/** The import resource; null when the provider has no billing import. */
export async function getBillingImport(
  providerId: string,
  signal?: AbortSignal
): Promise<BillingImport | null> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/billing-import',
    { params: { path: { provider_id: providerId } }, signal }
  );
  if (response.response.status === 404) return null;
  return unwrap(response);
}

/**
 * PUT replaces or creates the import. On create the API matches the
 * provider's etag; on replace it matches the import's own.
 */
export async function putBillingImport(
  providerId: string,
  etag: string,
  input: PutBillingImportInput,
  requestId: string
): Promise<BillingImport> {
  const response = await apiClient.PUT(
    '/api/v1/providers/{provider_id}/billing-import',
    {
      params: {
        path: { provider_id: providerId },
        header: { 'If-Match': etag, 'Idempotency-Key': requestId }
      },
      body: input
    }
  );
  return unwrap(response);
}

export async function deleteBillingImport(
  providerId: string,
  etag: string
): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v1/providers/{provider_id}/billing-import',
    {
      params: {
        path: { provider_id: providerId },
        header: { 'If-Match': etag }
      }
    }
  );
  ensureOk(response);
}

export async function runBillingImport(
  providerId: string,
  requestId: string
): Promise<BillingImport> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/billing-import/run',
    {
      params: {
        path: { provider_id: providerId },
        header: { 'Idempotency-Key': requestId }
      }
    }
  );
  return unwrap(response);
}
