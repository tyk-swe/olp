import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrap } from '#lib/management/http.js';

export type Branding = components['schemas']['InstallationBrandingResponse'];

export async function getBranding(): Promise<Branding> {
  return unwrap(await apiClient.GET('/api/v1/branding'));
}

export async function updateBranding(identity: Branding): Promise<Branding> {
  return unwrap(
    await apiClient.PUT('/api/v1/branding', {
      params: { header: { 'If-Match': identity.etag } },
      body: { name: identity.name, logo: identity.logo }
    })
  );
}
