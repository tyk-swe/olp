import { QueryClient } from '@tanstack/svelte-query';
import { flushSync, mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api/billing';
import { ApiProblem } from '$lib/api/http';
import type { Provider } from './api/providers';
import BillingImportProbe from './test/BillingImportProbe.svelte';

vi.mock('./api/billing', () => ({
  getBillingImport: vi.fn(),
  putBillingImport: vi.fn(),
  deleteBillingImport: vi.fn(),
  runBillingImport: vi.fn()
}));

it('creates, runs, removes and recreates an Azure import with a fresh write identity', async () => {
  const provider: Provider = {
    id: 'provider',
    etag: 'provider-etag',
    name: 'Azure',
    configuration: { kind: 'azure_openai', auth_mode: 'api_key' },
    state: 'draft',
    connector_ready: true,
    pending_activation: false,
    model_count: 0,
    enabled_model_count: 0,
    capability_count: 0,
    certified_capability_count: 0,
    project_id: null,
    project_name: null,
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString()
  };
  let current: api.BillingImport | null = null;
  let failNextSave = true;
  const committed = new Map<string, api.BillingImport>();
  vi.mocked(api.getBillingImport).mockImplementation(async () => current);
  vi.mocked(api.putBillingImport).mockImplementation(
    async (_id, _etag, input, requestId) => {
      if (failNextSave) {
        failNextSave = false;
        throw new Error('Save failed');
      }
      const prior = committed.get(requestId);
      if (prior) return prior;
      current = {
        provider_id: provider.id,
        source: 'azure_cost_management',
        enabled: true,
        scope: input.scope ?? {},
        credential_configured: true,
        etag: 'billing-etag',
        status: {
          next_import_at: null,
          last_import_at: null,
          last_success_at: null,
          last_error_code: null,
          imported_from: null,
          imported_through: null,
          source_revision: null
        }
      };
      committed.set(requestId, current);
      return current;
    }
  );
  vi.mocked(api.deleteBillingImport).mockImplementation(async () => {
    current = null;
  });
  vi.mocked(api.runBillingImport).mockImplementation(async () => current!);
  vi.stubGlobal('confirm', () => true);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(BillingImportProbe, {
    target: host,
    props: { client, provider }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.unstubAllGlobals();
  });
  await vi.waitFor(() => expect(host.textContent).toContain('Create import'));
  const fill = () => {
    for (const [id, value] of [
      [
        'billing-azure-scope',
        '/subscriptions/11111111-1111-4111-8111-111111111111'
      ],
      ['billing-azure-export', 'costs'],
      ['billing-azure-account', 'account'],
      ['billing-azure-container', 'exports'],
      ['billing-azure-auth', 'azure_client_secret']
    ]) {
      const field = host.querySelector<HTMLInputElement | HTMLSelectElement>(
        `#${id}`
      )!;
      field.value = value;
      field.dispatchEvent(
        new Event(id.endsWith('auth') ? 'change' : 'input', { bubbles: true })
      );
    }
    flushSync();
    for (const [id, value] of [
      ['billing-azure-tenant', 'tenant'],
      ['billing-azure-client', 'client'],
      ['billing-azure-secret', 'secret']
    ]) {
      const field = host.querySelector<HTMLInputElement>(`#${id}`)!;
      field.value = value;
      field.dispatchEvent(new Event('input', { bubbles: true }));
    }
    flushSync();
  };
  const submit = () =>
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  const click = (label: string) =>
    [...host.querySelectorAll('button')]
      .find((button) => button.textContent?.trim() === label)!
      .click();
  fill();
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Save failed'));
  expect(
    host.querySelector<HTMLInputElement>('#billing-azure-secret')?.value
  ).toBe('secret');
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Run now'));
  expect(host.querySelector('#billing-azure-secret')).toBeNull();
  click('Run now');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Import scheduled.')
  );
  const setField = (id: string, value: string) => {
    const field = host.querySelector<HTMLInputElement | HTMLSelectElement>(
      `#${id}`
    )!;
    field.value = value;
    field.dispatchEvent(
      new Event(field.tagName === 'SELECT' ? 'change' : 'input', {
        bubbles: true
      })
    );
    flushSync();
  };
  setField('billing-azure-export', 'local-export');
  setField('billing-azure-auth', 'azure_client_secret');
  setField('billing-azure-tenant', 'local-tenant');
  setField('billing-azure-client', 'local-client');
  setField('billing-azure-secret', 'local-secret');
  const remote = {
    ...current!,
    enabled: false,
    etag: 'remote-etag',
    scope: {
      billing_scope: '/subscriptions/22222222-2222-4222-8222-222222222222',
      export_name: 'remote-export',
      storage_account: 'remoteaccount',
      container: 'remotecontainer',
      prefix: 'remote-prefix',
      subscription_ids: ['33333333-3333-4333-8333-333333333333']
    }
  };
  current = remote;
  vi.mocked(api.putBillingImport).mockRejectedValueOnce(
    new ApiProblem({
      title: 'Changed elsewhere',
      status: 412,
      type: 'https://openllmproxy.dev/problems/etag_mismatch'
    })
  );
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('This item changed elsewhere.')
  );
  click('Reload');
  await vi.waitFor(() =>
    expect(host.textContent).not.toContain('This item changed elsewhere.')
  );
  expect(
    host.querySelector<HTMLInputElement>('#billing-azure-export')?.value
  ).toBe('remote-export');
  expect(
    host.querySelector<HTMLSelectElement>('#billing-azure-auth')?.value
  ).toBe('');
  setField('billing-azure-auth', 'azure_client_secret');
  for (const id of [
    'billing-azure-tenant',
    'billing-azure-client',
    'billing-azure-secret'
  ])
    expect(host.querySelector<HTMLInputElement>(`#${id}`)?.value).toBe('');
  setField('billing-azure-auth', '');
  submit();
  await vi.waitFor(() =>
    expect(api.putBillingImport).toHaveBeenLastCalledWith(
      'provider',
      'remote-etag',
      { enabled: false, scope: remote.scope },
      expect.any(String)
    )
  );
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Billing import saved.')
  );
  click('Remove');
  await vi.waitFor(() => expect(host.textContent).toContain('Create import'));
  fill();
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Run now'));
  const calls = vi.mocked(api.putBillingImport).mock.calls;
  expect(calls).toHaveLength(5);
  expect(calls[1][3]).toBe(calls[0][3]);
  expect(calls[4][3]).not.toBe(calls[3][3]);
});
