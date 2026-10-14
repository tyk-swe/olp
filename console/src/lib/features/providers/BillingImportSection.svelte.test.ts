// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  deleteBillingImport,
  getBillingImport,
  putBillingImport,
  runBillingImport,
  type BillingImport
} from '$lib/features/providers/api/billing';
import BillingImportProbe from './test/BillingImportProbe.svelte';
import type { Provider } from '$lib/features/providers/api/providers';

vi.mock('$lib/features/providers/api/billing', () => ({
  deleteBillingImport: vi.fn(),
  getBillingImport: vi.fn(),
  putBillingImport: vi.fn(),
  runBillingImport: vi.fn()
}));

const openaiProvider = {
  id: '01980000-0000-7000-8000-000000000501',
  name: 'OpenAI',
  etag: 'provider-etag-1',
  configuration: { kind: 'openai' }
} as unknown as Provider;

const geminiProvider = {
  ...openaiProvider,
  id: '01980000-0000-7000-8000-000000000502',
  configuration: { kind: 'gemini' }
} as unknown as Provider;

const configured: BillingImport = {
  provider_id: openaiProvider.id,
  source: 'openai_costs',
  enabled: true,
  scope: { project_ids: ['proj_aaa'] } as never,
  credential_configured: true,
  etag: 'import-etag-1',
  status: {
    next_import_at: '2026-10-09T01:00:00Z',
    last_import_at: '2026-10-08T01:00:00Z',
    last_success_at: '2026-10-08T01:00:00Z',
    last_error_code: 'credential',
    imported_from: '2026-10-01',
    imported_through: '2026-10-08'
  }
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

async function render(
  provider: Provider,
  canManage = true,
  supportsImport = true
) {
  component = mount(BillingImportProbe, {
    target: host,
    props: { client, provider, canManage }
  });
  flushSync();
  if (supportsImport) {
    await vi.waitFor(() =>
      expect(vi.mocked(getBillingImport)).toHaveBeenCalled()
    );
  }
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

it('renders nothing for providers without a billing source', async () => {
  await render(geminiProvider, true, false);
  expect(host.querySelector('section')).toBeNull();
  expect(getBillingImport).not.toHaveBeenCalled();
});

it('offers the create form with scope validation', async () => {
  vi.mocked(getBillingImport).mockResolvedValue(null);
  await render(openaiProvider);

  expect(host.textContent).toContain('admin key');
  const scope = host.querySelector<HTMLTextAreaElement>('#billing-scope')!;
  expect(scope.placeholder).toContain('proj_');
  scope.value = 'bogus-id';
  scope.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host.querySelector<HTMLButtonElement>('button[type="submit"]')!.click();
  flushSync();
  expect(host.textContent).toContain('is not a valid project ID');
  expect(putBillingImport).not.toHaveBeenCalled();
});

it('creates the import with the provider etag and an admin key', async () => {
  vi.mocked(getBillingImport).mockResolvedValue(null);
  vi.mocked(putBillingImport).mockResolvedValue(configured);
  await render(openaiProvider);

  const key = host.querySelector<HTMLInputElement>('#billing-admin-key')!;
  key.value = 'sk-admin-secret';
  key.dispatchEvent(new Event('input', { bubbles: true }));
  const scope = host.querySelector<HTMLTextAreaElement>('#billing-scope')!;
  scope.value = 'proj_aaa\nproj_bbb';
  scope.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host.querySelector<HTMLButtonElement>('button[type="submit"]')!.click();
  await vi.waitFor(() =>
    expect(vi.mocked(putBillingImport)).toHaveBeenCalled()
  );

  expect(putBillingImport).toHaveBeenCalledWith(
    openaiProvider.id,
    'provider-etag-1',
    expect.objectContaining({
      enabled: true,
      scope: { project_ids: ['proj_aaa', 'proj_bbb'] },
      credential: 'sk-admin-secret'
    }),
    expect.any(String)
  );
});

it('updates under the import etag, omitting a blank key', async () => {
  vi.mocked(getBillingImport).mockResolvedValue(configured);
  vi.mocked(putBillingImport).mockResolvedValue(configured);
  await render(openaiProvider);
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();

  expect(host.textContent).toContain('openai_costs');
  expect(host.textContent).toContain('the vendor rejected the admin key');
  const enabled = host.querySelector<HTMLInputElement>(
    'input[type="checkbox"]'
  )!;
  enabled.click();
  flushSync();
  host.querySelector<HTMLButtonElement>('button[type="submit"]')!.click();
  await vi.waitFor(() =>
    expect(vi.mocked(putBillingImport)).toHaveBeenCalled()
  );
  expect(putBillingImport).toHaveBeenCalledWith(
    openaiProvider.id,
    'import-etag-1',
    expect.objectContaining({
      enabled: false,
      scope: { project_ids: ['proj_aaa'] }
    }),
    expect.any(String)
  );
  expect(vi.mocked(putBillingImport).mock.calls[0]![2]).not.toHaveProperty(
    'credential'
  );
});

it('runs the importer and removes the import with confirmation', async () => {
  vi.mocked(getBillingImport).mockResolvedValue(configured);
  vi.mocked(runBillingImport).mockResolvedValue(configured);
  vi.mocked(deleteBillingImport).mockResolvedValue();
  const confirmSpy = vi.spyOn(globalThis, 'confirm').mockReturnValue(true);
  await render(openaiProvider);

  const run = [...host.querySelectorAll('button')].find(
    (b) => b.textContent === 'Run now'
  )!;
  run.click();
  await vi.waitFor(() =>
    expect(vi.mocked(runBillingImport)).toHaveBeenCalledWith(
      openaiProvider.id,
      expect.any(String)
    )
  );

  const remove = [...host.querySelectorAll('button')].find(
    (b) => b.textContent === 'Remove'
  )!;
  await vi.waitFor(() => expect(remove.disabled).toBe(false));
  remove.click();
  await vi.waitFor(() =>
    expect(vi.mocked(deleteBillingImport)).toHaveBeenCalledWith(
      openaiProvider.id,
      'import-etag-1'
    )
  );
  expect(confirmSpy).toHaveBeenCalled();
  confirmSpy.mockRestore();
});

it('read-only users see status without the form', async () => {
  vi.mocked(getBillingImport).mockResolvedValue(configured);
  await render(openaiProvider, false);

  expect(host.textContent).toContain('managed by connection managers');
  expect(host.querySelector('form')).toBeNull();
  expect(host.textContent).toContain('2026-10-08');
});
