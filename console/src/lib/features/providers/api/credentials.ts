import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { type CursorPage, unwrap, unwrapPage } from '$lib/api/http';
import { PROVIDER_CREDENTIAL_PAGE_SIZE } from '$lib/api/pageSizes';
import { collectCursorPages } from '$lib/api/pagination';
import type { Provider } from '$lib/features/providers/api/providers';

type Schemas = components['schemas'];

export type ProviderCredential = Schemas['CredentialResponse'];
export type ExternalCredentialReference =
  Schemas['ExternalCredentialReference'];
export type CredentialSlot = Schemas['CredentialSlot'];
export type CredentialSlotPool = Schemas['SlotList'];

export async function listCredentialSlots(
  providerId: string,
  signal?: AbortSignal
): Promise<CredentialSlotPool> {
  return unwrap(
    await apiClient.GET('/api/v1/providers/{provider_id}/credential-slots', {
      params: { path: { provider_id: providerId } },
      signal
    })
  );
}

export async function putCredentialSlot(
  providerId: string,
  etag: string,
  slot: Schemas['SlotWrite'],
  requestId: string
): Promise<void> {
  const slotId = slot.slot.id;
  if (slotId == null) throw new Error('A credential slot ID is required.');
  unwrap(
    await apiClient.PUT(
      '/api/v1/providers/{provider_id}/credential-slots/{slot_id}',
      {
        params: {
          path: { provider_id: providerId, slot_id: slotId },
          header: { 'If-Match': etag, 'Idempotency-Key': requestId }
        },
        body: slot
      }
    )
  );
}

export async function validateCredentialSlot(
  providerId: string,
  slotId: string,
  etag: string
): Promise<void> {
  unwrap(
    await apiClient.POST(
      '/api/v1/providers/{provider_id}/credential-slots/{slot_id}/validate',
      {
        params: { path: { provider_id: providerId, slot_id: slotId } },
        headers: { 'If-Match': etag }
      }
    )
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
  return unwrapPage(response);
}

export async function rotateProviderCredential(
  provider: Pick<Provider, 'id' | 'etag'>,
  secret: string | ExternalCredentialReference,
  requestId: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      },
      body:
        typeof secret === 'string'
          ? { credential: secret }
          : { credential_reference: secret }
    }
  );
  unwrap(response);
}

export async function revokeProviderCredential(
  provider: Pick<Provider, 'id' | 'etag'>,
  credentialId: string,
  requestId: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke',
    {
      params: {
        path: { provider_id: provider.id, credential_id: credentialId },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      }
    }
  );
  unwrap(response);
}
