import type { components } from '../schema';
import { apiClient } from '../client';
import { getAbortSignal, requireResponseData, type ReadSignal } from './shared';

type Schemas = components['schemas'];

export type OidcConfiguration = Schemas['OidcConfigurationResponse'];
export type OidcConfigurationInput = Schemas['OidcConfigurationRequest'];

export async function getOidcConfiguration(signal?: ReadSignal): Promise<OidcConfiguration | null> {
  const response = await apiClient.GET('/api/v1/oidc/configuration', {
    signal: getAbortSignal(signal)
  });
  if (response.response.status === 404) return null;
  return requireResponseData(response.data, response.error, response.response);
}

export async function putOidcConfiguration(
  input: OidcConfigurationInput,
  etag?: string
): Promise<OidcConfiguration> {
  const response = await apiClient.PUT('/api/v1/oidc/configuration', {
    params: { header: { 'If-Match': etag ?? null } },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function beginOidcLink(): Promise<string> {
  const response = await apiClient.POST('/api/v1/oidc/link');
  return requireResponseData(response.data, response.error, response.response).authorization_url;
}
