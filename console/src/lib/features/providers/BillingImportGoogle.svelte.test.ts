import { QueryClient } from '@tanstack/svelte-query';
import { flushSync, mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api/billing';
import type { Provider } from './api/providers';
import BillingImportProbe from './test/BillingImportProbe.svelte';

vi.mock('./api/billing', () => ({
  getBillingImport: vi.fn(),
  putBillingImport: vi.fn(),
  deleteBillingImport: vi.fn(),
  runBillingImport: vi.fn()
}));

it('validates, creates, runs, updates and removes a Google billing import', async () => {
  const provider: Provider = {
    id: 'provider',
    etag: 'provider-etag',
    name: 'Vertex',
    configuration: { kind: 'vertex_ai', auth_mode: 'adc' },
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
  vi.mocked(api.getBillingImport).mockImplementation(async () => current);
  vi.mocked(api.putBillingImport).mockImplementation(
    async (_id, _etag, input) => {
      current = {
        provider_id: provider.id,
        source: 'google_bigquery',
        enabled: input.enabled ?? true,
        scope: input.scope!,
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
      return current;
    }
  );
  vi.mocked(api.runBillingImport).mockImplementation(async () => current!);
  vi.mocked(api.deleteBillingImport).mockImplementation(async () => {
    current = null;
  });
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
  const fill = (id: string, value: string) => {
    const field = host.querySelector<
      HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement
    >(`#${id}`)!;
    field.value = value;
    field.dispatchEvent(
      new Event(field.tagName === 'SELECT' ? 'change' : 'input', {
        bubbles: true
      })
    );
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
  await vi.waitFor(() => expect(host.textContent).toContain('Create import'));
  for (const [id, value] of [
    ['billing-account', '123456-ABCDEF-123456'],
    ['billing-project', 'example.com:billing-project'],
    ['billing-dataset', 'billing_export'],
    ['billing-table', 'detailed_export'],
    ['billing-scope', 'example.com:workload-project'],
    [
      'billing-admin-key',
      '{"type":"service_account","private_key":"fixture-key"}'
    ]
  ])
    fill(id, value);
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Run now'));
  expect(api.putBillingImport).toHaveBeenLastCalledWith(
    'provider',
    'provider-etag',
    {
      enabled: true,
      scope: {
        billing_account_id: '123456-ABCDEF-123456',
        bigquery_project_id: 'example.com:billing-project',
        dataset_id: 'billing_export',
        table_id: 'detailed_export',
        project_ids: ['example.com:workload-project']
      },
      credential: {
        auth_mode: 'service_account',
        service_account: { type: 'service_account', private_key: 'fixture-key' }
      }
    },
    expect.any(String)
  );
  expect(host.querySelector('#billing-admin-key')).toBeNull();
  fill('billing-dataset', 'updated_export');
  submit();
  await vi.waitFor(() => expect(api.putBillingImport).toHaveBeenCalledTimes(2));
  expect(api.putBillingImport).toHaveBeenLastCalledWith(
    'provider',
    'billing-etag',
    expect.objectContaining({
      scope: expect.objectContaining({ dataset_id: 'updated_export' })
    }),
    expect.any(String)
  );
  const second = vi.mocked(api.putBillingImport).mock.calls[1][2];
  expect(second.credential).toBeUndefined();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Billing import saved.')
  );

  click('Remove');
  await vi.waitFor(() => expect(host.textContent).toContain('Create import'));
  expect(api.deleteBillingImport).toHaveBeenCalledWith(
    'provider',
    'billing-etag'
  );
});
