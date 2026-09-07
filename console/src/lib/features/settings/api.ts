import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

export type Setting = components['schemas']['SettingResponse'];

export async function listSettings(): Promise<Setting[]> {
  const { data, error, response } = await apiClient.GET('/api/v3/settings');
  return result(data, error, response).items;
}

export async function updateSetting(
  setting: Setting,
  value: string
): Promise<Setting> {
  const { data, error, response } = await apiClient.PUT(
    '/api/v3/settings/{key}',
    {
      params: {
        path: { key: setting.key },
        header: { 'If-Match': setting.etag }
      },
      body: { value }
    }
  );
  return result(data, error, response);
}
