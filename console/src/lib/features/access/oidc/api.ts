import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

type Schemas = components['schemas'];

export type OidcConfiguration = Schemas['OidcConfigurationResponse'];
export type OidcConfigurationInput = Schemas['OidcConfigurationRequest'];

export async function getOidcConfiguration(
  signal?: AbortSignal
): Promise<OidcConfiguration | null> {
  const response = await apiClient.GET('/api/v1/oidc/configuration', {
    signal
  });
  if (response.response.status === 404) return null;
  return unwrap(response);
}

export async function putOidcConfiguration(
  input: OidcConfigurationInput,
  etag?: string
): Promise<OidcConfiguration> {
  const response = await apiClient.PUT('/api/v1/oidc/configuration', {
    params: { header: { 'If-Match': etag ?? null } },
    body: input
  });
  return unwrap(response);
}

export async function beginOidcLink(): Promise<string> {
  const response = await apiClient.POST('/api/v1/oidc/link');
  return unwrap(response).authorization_url;
}
