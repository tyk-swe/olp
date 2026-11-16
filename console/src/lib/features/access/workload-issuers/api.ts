import { apiClient } from '#lib/management/client.js';
import { unwrap, unwrapPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';
export type Issuer = components['schemas']['WorkloadIssuer'];
export type IssuerWrite = components['schemas']['WorkloadIssuerWrite'];
export async function listIssuers(signal?: AbortSignal) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/workload-issuers', {
        params: { query: { cursor } },
        signal
      })
    )
  );
}
export async function saveIssuer(
  body: IssuerWrite,
  requestId: string,
  current?: Issuer
) {
  if (current)
    return unwrap(
      await apiClient.PUT('/api/v1/workload-issuers/{issuer_id}', {
        params: {
          path: { issuer_id: current.id },
          header: { 'If-Match': `"${current.etag}"` }
        },
        body
      })
    );
  return unwrap(
    await apiClient.POST('/api/v1/workload-issuers', {
      params: { header: { 'Idempotency-Key': requestId } },
      body
    })
  );
}
export function issuerInput(issuer: Issuer): IssuerWrite {
  const {
    name,
    issuer: url,
    jwks_url,
    enabled,
    audiences,
    algorithms,
    max_lifetime_seconds,
    disabled_key_ids,
    mappings
  } = issuer;
  return {
    name,
    issuer: url,
    jwks_url,
    enabled,
    audiences,
    algorithms,
    max_lifetime_seconds,
    disabled_key_ids,
    mappings
  };
}
