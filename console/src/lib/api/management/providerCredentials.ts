import type { components } from '../schema';
import { apiClient } from '../client';
import { pageResult, result, type CursorPage } from '../http';
import { PROVIDER_CREDENTIAL_PAGE_SIZE } from '../pageSizes';
import { collectCursorPages } from '../pagination';
import type { Provider } from './providers';

type Schemas = components['schemas'];

export type ProviderCredential = Schemas['CredentialResponse'];

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
