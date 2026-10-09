import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap } from '$lib/api/http';
import type { components } from '$lib/api/schema';
import type { RecentAuthenticationPurpose } from '../profile/api';
export type SAMLConfiguration = components['schemas']['SAMLConfiguration'];
export type SAMLWrite = components['schemas']['SAMLConfigurationWrite'];
export async function getSAML(signal?: AbortSignal) {
  const r = await apiClient.GET('/api/v1/saml/configuration', { signal });
  if (r.response.status === 404) return null;
  return unwrap(r);
}
export async function saveSAML(body: SAMLWrite, etag?: string) {
  return unwrap(
    await apiClient.PUT('/api/v1/saml/configuration', {
      body,
      params: { header: etag ? { 'If-Match': `"${etag}"` } : {} }
    })
  );
}
export async function importSAML(url: string) {
  return unwrap(await apiClient.POST('/api/v1/saml/import', { body: { url } }));
}
export async function listSAMLIdentities(signal?: AbortSignal) {
  return unwrap(
    await apiClient.GET('/api/v1/profile/saml-identities', { signal })
  );
}
export async function beginSAMLLogin(returnTo: string, signal?: AbortSignal) {
  return unwrap(
    await apiClient.POST('/api/v1/saml/login', {
      body: { return_to: returnTo },
      signal
    })
  ).authorization_url;
}
export async function beginSAMLLink() {
  return unwrap(await apiClient.POST('/api/v1/profile/saml/link'))
    .authorization_url;
}
export async function beginSAMLReauthentication(
  purpose: RecentAuthenticationPurpose,
  resourceId?: string
) {
  return unwrap(
    await apiClient.POST('/api/v1/profile/saml/reauthenticate', {
      body: { purpose, ...(resourceId ? { resource_id: resourceId } : {}) }
    })
  ).authorization_url;
}
export async function unlinkSAML(id: string) {
  ensureOk(
    await apiClient.DELETE('/api/v1/profile/saml-identities/{identity_id}', {
      params: { path: { identity_id: id } }
    })
  );
}
