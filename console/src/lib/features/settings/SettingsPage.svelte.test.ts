import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authenticationCapabilities } from '$lib/features/access/session/api';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import {
  listSettings,
  updateSetting,
  type Setting
} from '$lib/features/settings/api';
import SettingsPageProbe from './test/SettingsPageProbe.svelte';
import { listProviderKinds } from '$lib/features/providers/api/models';
import { listProviderVendors } from '$lib/features/providers/api/providers';
import { listPricingSources } from '$lib/features/usage/api/pricingSources';
import {
  createPricingRevision,
  listPricing,
  type PricingRevision
} from '$lib/features/usage/api/pricing';

vi.mock('$lib/features/access/budgets/api', () => ({
  getBudget: vi.fn().mockResolvedValue({
    policy: null,
    etag: '00000000-0000-4000-8000-000000000001',
    usage: {
      daily: { accrued: '0', window_ends_at: '2026-10-08T00:00:00Z' },
      monthly: { accrued: '0', window_ends_at: '2026-11-01T00:00:00Z' },
      unpriced_attempts: 0
    }
  }),
  putBudget: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/access/session/api', () => ({
  authenticationCapabilities: vi.fn()
}));
vi.mock('$lib/features/settings/api', async (original) => ({
  ...(await original<typeof import('$lib/features/settings/api')>()),
  listSettings: vi.fn(),
  updateSetting: vi.fn()
}));
vi.mock('$lib/features/settings/api/branding', async (original) => ({
  ...(await original<typeof import('$lib/features/settings/api/branding')>()),
  getBranding: vi.fn().mockResolvedValue({
    name: 'Test installation',
    logo: '',
    etag: 'branding-etag'
  }),
  updateBranding: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', () => ({
  listProviderKinds: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviderVendors: vi.fn()
}));
vi.mock('$lib/features/usage/api/pricingSources', async (original) => ({
  ...(await original<
    typeof import('$lib/features/usage/api/pricingSources')
  >()),
  listPricingSources: vi.fn()
}));
vi.mock('$lib/features/usage/api/pricing', () => ({
  createPricingRevision: vi.fn(),
  listPricing: vi.fn()
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
  payload_capture_active: false,
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
  client.setQueryData(sessionKeys.serviceCapabilities, capabilities);
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
    client.setQueryData(sessionKeys.serviceCapabilities, {
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
      expect(client.getQueryData(sessionKeys.serviceCapabilities)).toEqual({
        ...capabilities,
        local_login_enabled: enabled
      });
    });
    expect(updateSetting).toHaveBeenCalledWith(setting, String(enabled));
    expect(authenticationCapabilities).toHaveBeenCalledTimes(1);
  }
);

async function establishPricing() {
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    gateway_available: true,
    limits_enforced: true
  });
  client.setQueryData(
    routeKeys.policy(
      'installation',
      '00000000-0000-0000-0000-000000000000',
      ''
    ),
    { policy: {}, etag: 'policy-v1' }
  );
  vi.mocked(listPricingSources).mockResolvedValue([]);
  vi.mocked(listPricing).mockResolvedValue({ items: [], nextCursor: null });
  vi.mocked(listProviderVendors).mockResolvedValue([]);
  vi.mocked(listProviderKinds).mockResolvedValue([
    {
      kind: 'openai',
      label: 'OpenAI',
      description: '',
      auth_modes: [],
      default_auth_mode: 'api_key',
      fields: [],
      presets: []
    }
  ]);
  component = mount(SettingsPageProbe, { target: host, props: { client } });
  await vi.waitFor(() =>
    expect(host.querySelector<HTMLSelectElement>('#provider-kind')?.value).toBe(
      'openai'
    )
  );
  const model = host.querySelector<HTMLInputElement>('#price-model')!;
  model.value = 'chat';
  model.dispatchEvent(new Event('input', { bubbles: true }));
  const price = host.querySelector<HTMLInputElement>('#input-price')!;
  price.value = '2.00';
  price.dispatchEvent(new Event('input', { bubbles: true }));
  edit(auditSetting.key, '30');
}

function pricingSubmit() {
  host
    .querySelector('form.price-form')!
    .dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
  flushSync();
}

it('serializes pricing creation with setting saves and shares their feedback', async () => {
  const pending = Promise.withResolvers<Setting>();
  vi.mocked(updateSetting)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce({
      ...auditSetting,
      value: '45',
      etag: 'setting-v3'
    });
  vi.mocked(createPricingRevision).mockRejectedValue(
    new Error('Pricing rejected')
  );
  await establishPricing();
  saveButton(auditSetting.key).click();
  await vi.waitFor(() => expect(updateSetting).toHaveBeenCalledTimes(1));
  const priceButton = host.querySelector<HTMLButtonElement>(
    '.price-form button[type="submit"]'
  )!;
  expect(priceButton.disabled).toBe(true);
  pricingSubmit();
  expect(createPricingRevision).not.toHaveBeenCalled();
  pending.resolve({ ...auditSetting, value: '30', etag: 'setting-v2' });
  await vi.waitFor(() => expect(priceButton.disabled).toBe(false));
  expect(host.textContent).toContain('Audit retention (days) saved.');
  pricingSubmit();
  await vi.waitFor(() =>
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'Pricing rejected'
    )
  );
  expect(host.textContent).not.toContain('Audit retention (days) saved.');
  edit(auditSetting.key, '45');
  expect(saveButton(auditSetting.key).disabled).toBe(false);
  saveButton(auditSetting.key).click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Audit retention (days) saved.')
  );
  expect(host.textContent).not.toContain('Pricing rejected');
});

it('keeps pricing mounted and blocks setting saves throughout creation and capability changes', async () => {
  const pending = Promise.withResolvers<PricingRevision>();
  vi.mocked(createPricingRevision).mockReturnValue(pending.promise);
  await establishPricing();
  pricingSubmit();
  await vi.waitFor(() =>
    expect(createPricingRevision).toHaveBeenCalledTimes(1)
  );
  expect(saveButton(auditSetting.key).disabled).toBe(true);
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    gateway_available: false,
    limits_enforced: false
  });
  await vi.waitFor(() => expect(host.querySelector('.price-form')).toBeNull());
  expect(saveButton(auditSetting.key).disabled).toBe(true);
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    gateway_available: true,
    limits_enforced: true
  });
  await vi.waitFor(() =>
    expect(host.querySelector<HTMLInputElement>('#price-model')?.value).toBe(
      'chat'
    )
  );
  expect(
    host.querySelector<HTMLButtonElement>('.price-form button[type="submit"]')!
      .disabled
  ).toBe(true);
  saveButton(auditSetting.key).click();
  expect(updateSetting).not.toHaveBeenCalled();
  pending.resolve({
    id: 'pricing-revision',
    revision: 1,
    prices: [],
    effective_at: '2026-09-15T12:00:00Z',
    created_at: '2026-09-15T12:00:00Z',
    created_by: 'owner',
    source_name: null,
    source_snapshot_id: null
  });
  await vi.waitFor(() =>
    expect(saveButton(auditSetting.key).disabled).toBe(false)
  );
  expect(host.textContent).toContain('Pricing revision created.');
  expect(host.querySelector<HTMLInputElement>('#price-model')!.value).toBe('');
});

it('shows the deployment network restriction as an informational setting', async () => {
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    management_network_restricted: true
  });
  component = mount(SettingsPageProbe, { target: host, props: { client } });
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'Management access is restricted to client networks'
    )
  );
  expect(updateSetting).not.toHaveBeenCalled();
});

it('shows current budget periods separately from the pending time zone', async () => {
  const setting: Setting = {
    ...auditSetting,
    key: 'budgets.time_zone',
    value: 'Asia/Kathmandu',
    calendar: [
      {
        window_kind: 'day',
        time_zone: 'UTC',
        starts_at: '2026-10-08T00:00:00Z',
        ends_at: '2026-10-09T00:00:00Z',
        pending_time_zone: 'Asia/Kathmandu',
        effective_at: '2026-10-09T00:00:00Z'
      }
    ]
  };
  vi.mocked(listSettings).mockResolvedValue([setting]);
  component = mount(SettingsPageProbe, { target: host, props: { client } });
  await vi.waitFor(() =>
    expect(host.textContent?.replace(/\s+/g, ' ')).toContain(
      'Asia/Kathmandu takes effect'
    )
  );
  expect(host.textContent).toContain('day: UTC');
  expect(host.textContent).toContain('never resets current spend');
  expect(input(setting.key).value).toBe('Asia/Kathmandu');
});
