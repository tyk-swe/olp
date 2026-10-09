import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type Branding = components['schemas']['InstallationBrandingResponse'];
export const brandingKey = ['settings', 'branding'] as const;

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
