import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';
import type { components } from '$lib/api/schema';
export type Issuer = components['schemas']['WorkloadIssuer'];
export type IssuerWrite = components['schemas']['WorkloadIssuerWrite'];
export async function listIssuers(signal?: AbortSignal) {
  const items: Issuer[] = [];
  let cursor: string | undefined;
  do {
    const page = unwrap(
      await apiClient.GET('/api/v1/workload-issuers', {
        params: { query: { cursor } },
        signal
      })
    );
    items.push(...page.items);
    cursor = page.next_cursor ?? undefined;
  } while (cursor);
  return items;
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
