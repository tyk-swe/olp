import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authenticationCapabilities } from '$lib/features/access/session/auth';
import {
  listSettings,
  updateSetting,
  type Setting
} from '$lib/features/settings/api';
import SettingsPageProbe from './test/SettingsPageProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/access/session/auth', () => ({
  authenticationCapabilities: vi.fn()
}));
vi.mock('$lib/features/settings/api', async (original) => ({
  ...(await original<typeof import('$lib/features/settings/api')>()),
  listSettings: vi.fn(),
  updateSetting: vi.fn()
}));

const auditSetting: Setting = {
  key: 'retention.audit_days',
  value: '14',
  etag: 'setting-v1',
  updated_at: '2026-09-15T12:00:00Z',
  updated_by: 'owner-a'
};
const requestSetting: Setting = {
  ...auditSetting,
  key: 'retention.requests_days',
  value: '30',
  etag: 'request-v1'
};
const capabilities = {
  local_login_enabled: true,
  oidc_login_enabled: true,
  gateway_available: false,
  limits_enforced: false,
  notifications_active: false,
  retention_enforced: false
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(['service-capabilities'], capabilities);
  vi.mocked(listSettings).mockResolvedValue([auditSetting, requestSetting]);
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
});

function input(key: string) {
  const result = document.getElementById(`setting-${key}`);
  if (!(result instanceof HTMLInputElement))
    throw new Error(`Missing setting input: ${key}`);
  return result;
}

function edit(key: string, value: string) {
  const field = input(key);
  field.value = value;
  field.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function saveButton(key: string) {
  const result = input(key).closest('article')?.querySelector('button');
  if (!(result instanceof HTMLButtonElement))
    throw new Error(`Missing setting save button: ${key}`);
  return result;
}

it('preserves edits made during a save and serializes setting saves', async () => {
  const pending = Promise.withResolvers<Setting>();
  vi.mocked(updateSetting)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce({
      ...auditSetting,
      value: '45',
      etag: 'setting-v3'
    });
  component = mount(SettingsPageProbe, {
    target: host,
    props: { client }
  });
  await vi.waitFor(() => expect(input(auditSetting.key).value).toBe('14'));

  edit(auditSetting.key, '30');
  saveButton(auditSetting.key).click();
  await vi.waitFor(() => expect(updateSetting).toHaveBeenCalledTimes(1));

  edit(auditSetting.key, '45');
  edit(requestSetting.key, '60');
  expect(saveButton(requestSetting.key).disabled).toBe(true);
  saveButton(requestSetting.key).click();
  expect(updateSetting).toHaveBeenCalledTimes(1);

  pending.resolve({ ...auditSetting, value: '30', etag: 'setting-v2' });
  await vi.waitFor(() => {
    flushSync();
    expect(input(auditSetting.key).value).toBe('45');
    expect(saveButton(auditSetting.key).disabled).toBe(false);
  });
  expect(updateSetting).toHaveBeenNthCalledWith(
    1,
    expect.objectContaining({ etag: 'setting-v1' }),
    '30'
  );

  saveButton(auditSetting.key).click();
  await vi.waitFor(() => expect(updateSetting).toHaveBeenCalledTimes(2));
  expect(updateSetting).toHaveBeenNthCalledWith(
    2,
    expect.objectContaining({ etag: 'setting-v2' }),
    '45'
  );
});

it.each([true, false])(
  'refreshes cached capabilities after saving local sign-in as %s',
  async (enabled) => {
    const setting: Setting = {
      ...auditSetting,
      key: 'auth.local_login_enabled',
      value: String(!enabled)
    };
    client.setQueryData(['service-capabilities'], {
      ...capabilities,
      local_login_enabled: !enabled
    });
    vi.mocked(listSettings).mockResolvedValue([setting]);
    vi.mocked(updateSetting).mockResolvedValue({
      ...setting,
      value: String(enabled),
      etag: 'setting-v2'
    });
    vi.mocked(authenticationCapabilities).mockResolvedValue({
      ...capabilities,
      local_login_enabled: enabled
    });
    component = mount(SettingsPageProbe, { target: host, props: { client } });
    let field: HTMLSelectElement;
    await vi.waitFor(() => {
      const element = document.getElementById(`setting-${setting.key}`);
      expect(element).toBeInstanceOf(HTMLSelectElement);
      field = element as HTMLSelectElement;
    });
    expect(authenticationCapabilities).not.toHaveBeenCalled();

    field!.value = String(enabled);
    field!.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    field!.closest('article')!.querySelector('button')!.click();

    await vi.waitFor(() => {
      expect(host.textContent).toContain('Local password sign-in saved.');
      expect(client.getQueryData(['service-capabilities'])).toEqual({
        ...capabilities,
        local_login_enabled: enabled
      });
    });
    expect(updateSetting).toHaveBeenCalledWith(setting, String(enabled));
    expect(authenticationCapabilities).toHaveBeenCalledTimes(1);
  }
);
