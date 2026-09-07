import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

type Schemas = components['schemas'];

export type OidcConfiguration = Schemas['OidcConfigurationResponse'];
export type OidcConfigurationInput = Schemas['OidcConfigurationRequest'];

export async function getOidcConfiguration(
  signal?: AbortSignal
): Promise<OidcConfiguration | null> {
  const response = await apiClient.GET('/api/v3/oidc/configuration', {
    signal
  });
  if (response.response.status === 404) return null;
  return result(response.data, response.error, response.response);
}

export async function putOidcConfiguration(
  input: OidcConfigurationInput,
  etag?: string
): Promise<OidcConfiguration> {
  const response = await apiClient.PUT('/api/v3/oidc/configuration', {
    params: { header: { 'If-Match': etag ?? null } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function beginOidcLink(): Promise<string> {
  const response = await apiClient.POST('/api/v3/oidc/link');
  return result(response.data, response.error, response.response)
    .authorization_url;
}
