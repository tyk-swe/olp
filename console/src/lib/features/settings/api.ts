import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, ifMatch } from '#lib/management/http.js';
import type { Branding } from '#lib/shell/branding/branding.js';

export type Setting = components['schemas']['SettingResponse'];
export type BrandingUpdate =
  components['schemas']['UpdateInstallationBrandingRequest'];

export async function listSettings(): Promise<Setting[]> {
  const { data, error, response } = await apiClient.GET('/api/v1/settings');
  return unwrap({ data, error, response }).items;
}

export async function updateSetting(
  setting: Setting,
  value: string
): Promise<Setting> {
  const { data, error, response } = await apiClient.PUT(
    '/api/v1/settings/{key}',
    {
      params: {
        path: { key: setting.key },
        header: ifMatch(setting.etag)
      },
      body: { value }
    }
  );
  return unwrap({ data, error, response });
}

export async function updateBranding(
  update: BrandingUpdate,
  etag: string
): Promise<Branding> {
  return unwrap(
    await apiClient.PUT('/api/v1/branding', {
      params: { header: ifMatch(etag) },
      body: update
    })
  );
}
