import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  unwrapAuthorizationUrl,
  ifMatch
} from '#lib/management/http.js';
import type { components } from '#lib/management/schema.js';
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
      params: { header: etag ? ifMatch(etag) : {} }
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
  const { authorization_url } = unwrap(
    await apiClient.POST('/api/v1/saml/login', {
      body: { return_to: returnTo },
      signal
    })
  );
  return unwrapAuthorizationUrl(
    authorization_url,
    'The SAML authorization response is invalid'
  );
}
export async function beginSAMLLink() {
  const { authorization_url } = unwrap(
    await apiClient.POST('/api/v1/profile/saml/link')
  );
  return unwrapAuthorizationUrl(
    authorization_url,
    'The SAML authorization response is invalid'
  );
}
export async function unlinkSAML(id: string) {
  ensureOk(
    await apiClient.DELETE('/api/v1/profile/saml-identities/{identity_id}', {
      params: { path: { identity_id: id } }
    })
  );
}
