import { createQuery } from '@tanstack/svelte-query';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap } from '#lib/management/http.js';

export type Branding = components['schemas']['InstallationBrandingResponse'];

export const brandingKey = ['branding'] as const;
export const productName = 'OpenLLMProxy';

export async function getBranding(): Promise<Branding> {
  return unwrap(await apiClient.GET('/api/v1/branding'));
}

export function useBranding() {
  return createQuery(() => ({ queryKey: brandingKey, queryFn: getBranding }));
}
