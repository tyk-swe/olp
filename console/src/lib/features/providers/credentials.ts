import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import type { Provider } from '#lib/domain/providers/api.js';

type Schemas = components['schemas'];

export type ExternalCredentialReference =
  Schemas['ExternalCredentialReference'];

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
          header: mutationHeaders(etag, requestId)
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
        headers: ifMatch(etag)
      }
    )
  );
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
        header: mutationHeaders(provider.etag, requestId)
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
        header: mutationHeaders(provider.etag, requestId)
      }
    }
  );
  unwrap(response);
}

export const deleteCredentialSlot = async (
  providerId: string,
  slotId: string,
  etag: string,
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE(
      '/api/v1/providers/{provider_id}/credential-slots/{slot_id}',
      {
        params: {
          path: { provider_id: providerId, slot_id: slotId },
          header: mutationHeaders(etag, requestId)
        }
      }
    )
  );
