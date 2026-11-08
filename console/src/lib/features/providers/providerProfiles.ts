import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrap, unwrapPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import { nativeObject } from '#lib/nativeJson.js';

type Schemas = components['schemas'];
export type ProviderProfile = Schemas['ProviderProfile'];
/** The installed provider plugin that supplies a plugin profile. */
export type ProviderProfilePlugin = Schemas['ProviderProfilePlugin'];
export type NetworkCredential = Schemas['NetworkCredentialResponse'];
export type FieldSchema = {
  type?: string | string[];
  title?: string;
  description?: string;
  format?: string;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  pattern?: string;
  enum?: unknown[];
  properties?: Record<string, FieldSchema>;
  additionalProperties?: boolean | FieldSchema;
  $ref?: string;
};
export type ConfigurationSchemas = Record<string, FieldSchema>;

export async function listProviderProfiles(
  signal?: AbortSignal
): Promise<ProviderProfile[]> {
  return unwrap(await apiClient.GET('/api/v1/provider-profiles', { signal }))
    .items;
}

export async function getConfigurationSchemas(
  signal?: AbortSignal
): Promise<ConfigurationSchemas> {
  const document = unwrap(
    await apiClient.GET('/api/v1/openapi.json', { signal })
  );
  if (
    !nativeObject(document) ||
    !nativeObject(document.components) ||
    !nativeObject(document.components.schemas)
  )
    throw new Error('The management configuration schemas are unavailable.');
  return document.components.schemas as ConfigurationSchemas;
}

export async function listNetworkCredentials(
  providerId: string,
  signal?: AbortSignal
): Promise<NetworkCredential[]> {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET(
        '/api/v1/providers/{provider_id}/network-credentials',
        {
          params: {
            path: { provider_id: providerId },
            query: { cursor, limit: 200 }
          },
          signal
        }
      )
    )
  );
}

export async function createNetworkCredential(
  providerId: string,
  etag: string,
  credential: string,
  requestId: string
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/providers/{provider_id}/network-credentials',
      {
        params: {
          path: { provider_id: providerId },
          header: { 'If-Match': etag, 'Idempotency-Key': requestId }
        },
        body: { credential }
      }
    )
  );
}

export async function revokeNetworkCredential(
  providerId: string,
  etag: string,
  credentialId: string,
  requestId: string
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/providers/{provider_id}/network-credentials/{credential_id}/revoke',
      {
        params: {
          path: { provider_id: providerId, credential_id: credentialId },
          header: { 'If-Match': etag, 'Idempotency-Key': requestId }
        }
      }
    )
  );
}
