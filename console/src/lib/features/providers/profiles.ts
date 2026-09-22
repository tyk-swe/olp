import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';
import { pageResult } from '$lib/api/http';
import { nativeObject } from '$lib/json/nativeJson';

type Schemas = components['schemas'];
export type ProviderProfile = Schemas['ProviderProfile'];
export type NetworkCredential = Schemas['NetworkCredentialResponse'];
export type FieldSchema = {
  type?: string | string[];
  title?: string;
  description?: string;
  format?: string;
  minimum?: number;
  maximum?: number;
  maxLength?: number;
  enum?: unknown[];
  properties?: Record<string, FieldSchema>;
  additionalProperties?: boolean | FieldSchema;
  $ref?: string;
};
export type ConfigurationSchemas = Record<string, FieldSchema>;

export async function listProviderProfiles(
  signal?: AbortSignal
): Promise<ProviderProfile[]> {
  const response = await apiClient.GET('/api/v3/provider-profiles', { signal });
  return result(response.data, response.error, response.response).items;
}
export async function getConfigurationSchemas(
  signal?: AbortSignal
): Promise<ConfigurationSchemas> {
  const response = await apiClient.GET('/api/v3/openapi.json', { signal });
  const document = result(response.data, response.error, response.response);
  if (
    !nativeObject(document) ||
    !nativeObject(document.components) ||
    !nativeObject(document.components.schemas)
  )
    throw new Error('The management configuration schemas are unavailable.');
  return document.components.schemas as ConfigurationSchemas;
}
export function schemaProperties(schema: unknown): Record<string, FieldSchema> {
  if (!nativeObject(schema) || !nativeObject(schema.properties)) return {};
  return schema.properties as Record<string, FieldSchema>;
}
export function operationFields(
  profile: ProviderProfile,
  operation: string
): Record<string, FieldSchema> {
  return schemaProperties(
    schemaProperties(profile.default_schemas[operation]).values
  );
}
export async function listNetworkCredentials(
  providerId: string,
  signal?: AbortSignal
): Promise<NetworkCredential[]> {
  return collectCursorPages(async (cursor) => {
    const response = await apiClient.GET(
      '/api/v3/providers/{provider_id}/network-credentials',
      {
        params: {
          path: { provider_id: providerId },
          query: { cursor, limit: 200 }
        },
        signal
      }
    );
    return pageResult(result(response.data, response.error, response.response));
  });
}
export async function createNetworkCredential(
  providerId: string,
  etag: string,
  credential: string
) {
  const response = await apiClient.POST(
    '/api/v3/providers/{provider_id}/network-credentials',
    {
      params: {
        path: { provider_id: providerId },
        header: { 'If-Match': etag, 'Idempotency-Key': crypto.randomUUID() }
      },
      body: { credential }
    }
  );
  return result(response.data, response.error, response.response);
}
export async function revokeNetworkCredential(
  providerId: string,
  etag: string,
  credentialId: string
) {
  const response = await apiClient.POST(
    '/api/v3/providers/{provider_id}/network-credentials/{credential_id}/revoke',
    {
      params: {
        path: { provider_id: providerId, credential_id: credentialId },
        header: { 'If-Match': etag, 'Idempotency-Key': crypto.randomUUID() }
      }
    }
  );
  return result(response.data, response.error, response.response);
}
