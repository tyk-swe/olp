import { createQuery } from '@tanstack/svelte-query';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrap } from '#lib/management/http.js';

export type Branding = components['schemas']['InstallationBrandingResponse'];
export type BrandingUpdate =
  components['schemas']['UpdateInstallationBrandingRequest'];

export const brandingKey = ['branding'] as const;
export const productName = 'OpenLLMProxy';

export async function getBranding(): Promise<Branding> {
  return unwrap(await apiClient.GET('/api/v1/branding'));
}

export async function updateBranding(
  update: BrandingUpdate,
  etag: string
): Promise<Branding> {
  return unwrap(
    await apiClient.PUT('/api/v1/branding', {
      params: { header: { 'If-Match': etag } },
      body: update
    })
  );
}

export function useBranding() {
  return createQuery(() => ({ queryKey: brandingKey, queryFn: getBranding }));
}
