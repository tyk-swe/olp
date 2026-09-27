import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result, type CursorPage } from '$lib/api/http';
import { PROVIDER_CREDENTIAL_PAGE_SIZE } from '$lib/api/pageSizes';
import { collectCursorPages } from '$lib/api/pagination';
import type { Provider } from '$lib/features/providers/api';

type Schemas = components['schemas'];

export type ProviderCredential = Schemas['CredentialResponse'];

/** Reports whether the credential version with the id is a live grant
 * enrolled through the plugin build with the digest, which a provider pinning
 * that build authenticates with: its grant hasn't lapsed and the version isn't
 * revoked. */
export function isLiveGrant(
  credentials: ProviderCredential[] | undefined,
  id: string | null | undefined,
  pluginDigest: string | undefined
): boolean {
  const credential = credentials?.find((candidate) => candidate.id === id);
  return Boolean(
    credential?.grant &&
    !credential.revoked_at &&
    !credential.grant.lapsed_at &&
    credential.grant.plugin_digest === pluginDigest
  );
}

export async function listProviderCredentials(
  id: string,
  signal?: AbortSignal
): Promise<ProviderCredential[]> {
  return collectCursorPages((cursor) =>
    listProviderCredentialPage(id, cursor, signal)
  );
}

async function listProviderCredentialPage(
  id: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderCredential>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/credentials',
    {
      params: {
        path: { provider_id: id },
        query: { cursor, limit: PROVIDER_CREDENTIAL_PAGE_SIZE }
      },
      signal
    }
  );
  return pageResult(result(response.data, response.error, response.response));
}

export async function rotateProviderCredential(
  provider: Provider,
  secret: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      },
      body: { credential: secret }
    }
  );
  result(response.data, response.error, response.response);
}

export async function revokeProviderCredential(
  provider: Provider,
  credentialId: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke',
    {
      params: {
        path: { provider_id: provider.id, credential_id: credentialId },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  result(response.data, response.error, response.response);
}
