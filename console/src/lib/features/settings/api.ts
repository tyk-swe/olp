import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type Setting = components['schemas']['SettingResponse'];

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
        header: { 'If-Match': setting.etag }
      },
      body: { value }
    }
  );
  return unwrap({ data, error, response });
}
