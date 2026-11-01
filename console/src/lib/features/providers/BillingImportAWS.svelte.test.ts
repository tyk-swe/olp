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

it('validates, creates, runs, updates and removes a Bedrock billing import', async () => {
  const provider: Provider = {
    id: 'provider',
    etag: 'provider-etag',
    name: 'Bedrock',
    configuration: { kind: 'bedrock', auth_mode: 'static' },
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
        source: 'aws_cur',
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
          source_revision: 'assembly-2'
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
    ['aws-payer', '111122223333'],
    ['aws-bucket', 'billing-report-bucket'],
    ['aws-prefix', 'billing/cur'],
    ['aws-region', 'us-east-1'],
    ['aws-linked', 'invalid'],
    ['aws-access', 'access-key'],
    ['aws-secret', 'secret-key'],
    ['aws-token', 'session-token']
  ])
    fill(id, value);
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('12-digit linked account IDs')
  );
  expect(api.putBillingImport).not.toHaveBeenCalled();
  fill('aws-linked', '444455556666\n777788889999');
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Run now'));
  expect(api.putBillingImport).toHaveBeenLastCalledWith(
    'provider',
    'provider-etag',
    {
      enabled: true,
      scope: {
        payer_account_id: '111122223333',
        bucket: 'billing-report-bucket',
        report_prefix: 'billing/cur',
        region: 'us-east-1',
        linked_account_ids: ['444455556666', '777788889999']
      },
      credential: {
        auth_mode: 'static',
        access_key_id: 'access-key',
        secret_access_key: 'secret-key',
        session_token: 'session-token'
      }
    },
    expect.any(String)
  );
  expect(host.querySelector('#aws-secret')).toBeNull();
  expect(host.textContent).toContain('assembly-2');
  click('Run now');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Import scheduled.')
  );
  expect(api.runBillingImport).toHaveBeenCalledWith(
    'provider',
    expect.any(String)
  );
  const replace = [...host.querySelectorAll('label')]
    .find((label) => label.textContent?.includes('Replace billing credential'))!
    .querySelector('input')!;
  replace.click();
  flushSync();
  fill('aws-auth', 'workload');
  expect(host.querySelector('#aws-secret')).toBeNull();
  submit();
  await vi.waitFor(() => expect(api.putBillingImport).toHaveBeenCalledTimes(2));
  expect(api.putBillingImport).toHaveBeenLastCalledWith(
    'provider',
    'billing-etag',
    expect.objectContaining({ credential: { auth_mode: 'workload' } }),
    expect.any(String)
  );
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Billing import saved.')
  );
  fill('aws-bucket', 'local-bucket');
  replace.click();
  flushSync();
  fill('aws-auth', 'static');
  fill('aws-access', 'local-access');
  fill('aws-secret', 'local-secret');
  fill('aws-token', 'local-token');
  const remote = {
    ...current!,
    enabled: false,
    etag: 'remote-etag',
    scope: {
      payer_account_id: '999988887777',
      bucket: 'remote-bucket',
      report_prefix: 'remote/report',
      region: 'remote-region',
      linked_account_ids: ['222233334444']
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
  expect(host.querySelector<HTMLInputElement>('#aws-bucket')?.value).toBe(
    'remote-bucket'
  );
  expect(replace.checked).toBe(false);
  replace.click();
  flushSync();
  for (const id of ['aws-access', 'aws-secret', 'aws-token'])
    expect(host.querySelector<HTMLInputElement>(`#${id}`)?.value).toBe('');
  replace.click();
  flushSync();
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
  expect(api.deleteBillingImport).toHaveBeenCalledWith(
    'provider',
    'billing-etag'
  );
});
