import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';

export type Setting = components['schemas']['SettingResponse'];

export async function listSettings(): Promise<Setting[]> {
  const { data, error, response } = await apiClient.GET('/api/v1/settings');
  return result(data, error, response).items;
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
        header: { 'If-Match': setting.etag }
      },
      body: { value }
    }
  );
  return result(data, error, response);
}
