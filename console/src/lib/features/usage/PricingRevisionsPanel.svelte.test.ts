import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authenticationCapabilities } from '$lib/features/access/session/api';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import { listProviderKinds } from '$lib/features/providers/api/models';
import { listProviderVendors } from '$lib/features/providers/api/providers';
import {
  createPricingRevision,
  listPricing,
  type PricingRevision
} from './api/pricing';
import { pricingKeys } from './pricingKeys';
import PricingRevisionsProbe from './test/PricingRevisionsProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('$lib/features/access/session/api', () => ({
  authenticationCapabilities: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', () => ({
  listProviderKinds: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviderVendors: vi.fn()
}));
vi.mock('./api/pricing', () => ({
  createPricingRevision: vi.fn(),
  listPricing: vi.fn()
}));

const capabilities = {
  local_login_enabled: true,
  oidc_login_enabled: false,
  gateway_available: true,
  limits_enforced: true,
  notifications_active: false,
  payload_capture_active: false,
  retention_enforced: true
};
const revision: PricingRevision = {
  id: 'price-revision',
  revision: 2,
  created_at: '2026-09-15T12:00:00Z',
  effective_at: '2026-09-15T12:00:00Z',
  created_by: 'owner',
  source_name: null,
  source_snapshot_id: null,
  prices: [
    {
      provider_kind: 'openai',
      model: 'chat',
      operation: 'generation',
      input_per_million: '0.00012500',
      currency: 'USD'
    }
  ]
};
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
const onBusyChange = vi.fn();
const onFeedback = vi.fn();

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(useRole).mockReturnValue({
    can: () => true,
    allows: () => true,
    role: 'owner',
    globalScope: true,
    user: null
  });
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(sessionKeys.serviceCapabilities, capabilities);
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
  vi.mocked(listProviderVendors).mockResolvedValue([]);
  vi.mocked(listPricing).mockResolvedValue({
    items: [revision],
    nextCursor: null
  });
  vi.mocked(createPricingRevision).mockResolvedValue(revision);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function establish(blocked = false) {
  component = mount(PricingRevisionsProbe, {
    target: host,
    props: { client, blocked, onBusyChange, onFeedback }
  });
  flushSync();
}

function field(id: string) {
  const element = host.querySelector<HTMLInputElement | HTMLSelectElement>(
    `#${id}`
  );
  if (!element) throw new Error(`Missing field ${id}`);
  return element;
}

function edit(id: string, value: string) {
  const element = field(id);
  element.value = value;
  element.dispatchEvent(
    new Event(element instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
  flushSync();
}

function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
  flushSync();
}

function createButton() {
  return host.querySelector<HTMLButtonElement>('button[type="submit"]')!;
}

async function ready() {
  await vi.waitFor(() => expect(field('provider-kind').value).toBe('openai'));
  edit('price-model', ' model-a ');
  edit('input-price', ' 0.00012500 ');
  edit('effective-at', '2026-09-15T09:30');
}

it('submits exact decimals, zero and null rates, then refreshes revisions', async () => {
  establish();
  await ready();
  edit('cached-input-price', '0');
  edit('cache-write-price', '0.004500');
  edit('cache-write-5m-price', '0.006');
  edit('cache-write-1h-price', '0.012');
  edit('output-price', '10.00');
  edit('currency', 'eur');
  submit();
  await vi.waitFor(() => expect(onBusyChange).toHaveBeenLastCalledWith(false));
  expect(createPricingRevision).toHaveBeenCalledWith(
    new Date('2026-09-15T09:30').toISOString(),
    [
      {
        provider_kind: 'openai',
        vendor_id: null,
        provider_id: null,
        model: 'model-a',
        operation: 'generation',
        input_per_million: '0.00012500',
        cached_input_per_million: '0',
        cache_write_input_per_million: '0.004500',
        cache_write_5m_input_per_million: '0.006',
        cache_write_1h_input_per_million: '0.012',
        output_per_million: '10.00',
        unit_price: null,
        audio_input_per_million: null,
        cached_audio_input_per_million: null,
        audio_output_per_million: null,
        image_input_per_million: null,
        cached_image_input_per_million: null,
        image_output_per_million: null,
        per_request: null,
        batch_multiplier: null,
        tool_calls: null,
        tiers: null,
        windows: null,
        currency: 'EUR'
      }
    ],
    expect.any(String)
  );
  expect(onFeedback).toHaveBeenLastCalledWith({
    status:
      'Pricing revision created. New usage will use the effective revision.',
    error: ''
  });
  expect(listPricing).toHaveBeenCalledTimes(2);
  expect(field('price-model').value).toBe('');
  expect(field('input-price').value).toBe('');
  expect(host.textContent).toContain('0.00012500');
  expect(host.textContent).toContain('Billed as input');
});

it.each([
  ['-0.1', 'Enter a non-negative decimal number.'],
  ['', 'Enter at least one price.']
])('reports invalid price %s without submitting', async (value, error) => {
  establish();
  await ready();
  edit('input-price', value);
  submit();
  expect(createPricingRevision).not.toHaveBeenCalled();
  expect(onFeedback).toHaveBeenLastCalledWith({ status: '', error });
  expect(onBusyChange).toHaveBeenLastCalledWith(false);
  expect(createButton().disabled).toBe(false);
});

it('keeps later edits while a submission is pending and refuses duplicate submissions', async () => {
  const pending = Promise.withResolvers<PricingRevision>();
  vi.mocked(createPricingRevision).mockReturnValue(pending.promise);
  establish();
  await ready();
  submit();
  expect(onBusyChange).toHaveBeenLastCalledWith(true);
  expect(createButton().disabled).toBe(true);
  edit('price-model', 'model-b');
  edit('input-price', '2.000');
  edit('cached-input-price', '0.20');
  edit('effective-at', '2026-09-16T10:00');
  submit();
  expect(createPricingRevision).toHaveBeenCalledTimes(1);
  pending.resolve(revision);
  await vi.waitFor(() => expect(createButton().disabled).toBe(false));
  expect(field('price-model').value).toBe('model-b');
  expect(field('input-price').value).toBe('2.000');
  expect(field('cached-input-price').value).toBe('0.20');
  expect(field('effective-at').value).toBe('2026-09-16T10:00');
});

it('reports a submission failure, preserves the form, and releases the save lock', async () => {
  vi.mocked(createPricingRevision).mockRejectedValue(
    new Error('Pricing is unavailable')
  );
  establish();
  await ready();
  submit();
  await vi.waitFor(() =>
    expect(onFeedback).toHaveBeenLastCalledWith({
      status: '',
      error: 'Pricing is unavailable'
    })
  );
  expect(onBusyChange).toHaveBeenLastCalledWith(false);
  expect(field('price-model').value).toBe(' model-a ');
  expect(field('input-price').value).toBe(' 0.00012500 ');
  expect(createButton().disabled).toBe(false);
  expect(listPricing).toHaveBeenCalledTimes(1);
});

it('allows read-only roles to list revisions while refusing creation', async () => {
  vi.mocked(useRole).mockReturnValue({
    can: () => false,
    allows: () => false,
    role: 'viewer',
    globalScope: true,
    user: null
  });
  establish();
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 2'));
  expect(field('price-model').disabled).toBe(true);
  expect(createButton().disabled).toBe(true);
  expect(host.textContent).toContain('but not create them');
  submit();
  expect(createPricingRevision).not.toHaveBeenCalled();
  expect(onBusyChange).not.toHaveBeenCalled();
});

it('blocks pricing creation during another settings save', async () => {
  establish(true);
  await ready();
  expect(createButton().disabled).toBe(true);
  submit();
  expect(createPricingRevision).not.toHaveBeenCalled();
  expect(onBusyChange).not.toHaveBeenCalled();
});

it('gates queries on installation capabilities and preserves edits while hidden', async () => {
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    gateway_available: false,
    limits_enforced: false
  });
  establish();
  expect(host.querySelector('form')).toBeNull();
  expect(listProviderKinds).not.toHaveBeenCalled();
  expect(listProviderVendors).not.toHaveBeenCalled();
  expect(listPricing).not.toHaveBeenCalled();
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    limits_enforced: false
  });
  await ready();
  expect(listProviderVendors).toHaveBeenCalledTimes(1);
  expect(listPricing).not.toHaveBeenCalled();
  expect(createButton().disabled).toBe(true);
  expect(host.textContent).toContain('Pricing revisions become available');
  client.setQueryData(sessionKeys.serviceCapabilities, {
    ...capabilities,
    gateway_available: false,
    limits_enforced: false
  });
  await vi.waitFor(() => expect(host.querySelector('form')).toBeNull());
  client.setQueryData(sessionKeys.serviceCapabilities, capabilities);
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 2'));
  expect(field('price-model').value).toBe(' model-a ');
  expect(field('input-price').value).toBe(' 0.00012500 ');
  expect(createButton().disabled).toBe(false);
});

it('does not start pricing queries while installation capabilities are unavailable', async () => {
  client.removeQueries({ queryKey: sessionKeys.serviceCapabilities });
  const pending = Promise.withResolvers<typeof capabilities>();
  vi.mocked(authenticationCapabilities).mockReturnValue(pending.promise);
  establish();
  expect(host.querySelector('form')).toBeNull();
  pending.reject(new Error('Capabilities unavailable'));
  await vi.waitFor(() =>
    expect(client.getQueryState(sessionKeys.serviceCapabilities)?.status).toBe(
      'error'
    )
  );
  expect(listProviderKinds).not.toHaveBeenCalled();
  expect(listProviderVendors).not.toHaveBeenCalled();
  expect(listPricing).not.toHaveBeenCalled();
});

it('disables creation when provider capabilities fail to load', async () => {
  vi.mocked(listProviderKinds).mockRejectedValue(
    new Error('No provider capabilities')
  );
  establish();
  await vi.waitFor(() =>
    expect(host.textContent).toMatch(/pricing changes are\s+disabled/)
  );
  expect(createButton().disabled).toBe(true);
  expect(field('provider-kind').disabled).toBe(true);
});

it('pages through revisions and returns to the refreshed first page after creation', async () => {
  vi.mocked(listPricing).mockImplementation(async (cursor) => ({
    items: [
      { ...revision, id: cursor ? 'older' : 'latest', revision: cursor ? 1 : 2 }
    ],
    nextCursor: cursor ? null : 'older-page'
  }));
  establish();
  await ready();
  function pageButton(label: string) {
    return [...host.querySelectorAll<HTMLButtonElement>('nav button')].find(
      (button) => button.textContent?.trim() === label
    )!;
  }
  await vi.waitFor(() => expect(host.textContent).toContain('Page 1'));
  pageButton('Next').click();
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 1'));
  expect(listPricing).toHaveBeenCalledWith('older-page');
  expect(host.textContent).toContain('Page 2');
  expect(pageButton('Next').disabled).toBe(true);
  pageButton('Previous').click();
  await vi.waitFor(() => expect(host.textContent).toContain('Page 1'));
  pageButton('Next').click();
  await vi.waitFor(() => expect(host.textContent).toContain('Page 2'));
  submit();
  await vi.waitFor(() => {
    expect(onBusyChange).toHaveBeenLastCalledWith(false);
    expect(host.textContent).toContain('Page 1');
  });
  expect(listPricing).toHaveBeenLastCalledWith(undefined);
  expect(pageButton('Previous').disabled).toBe(true);
});

it('shows the created revision on the first page after paging away', async () => {
  let created = false;
  vi.mocked(createPricingRevision).mockImplementation(async () => {
    created = true;
    return revision;
  });
  vi.mocked(listPricing).mockImplementation(async (cursor) => ({
    items: [
      {
        ...revision,
        id: cursor ? 'older' : 'latest',
        revision: cursor ? 1 : created ? 3 : 2
      }
    ],
    nextCursor: cursor ? null : 'older-page'
  }));
  establish();
  await ready();
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 2'));
  [...host.querySelectorAll<HTMLButtonElement>('nav button')]
    .find((button) => button.textContent?.trim() === 'Next')!
    .click();
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 1'));
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('Revision 3'));
  // The older page was never overwritten with first-page data.
  expect(
    client.getQueryData<{ items: PricingRevision[] }>(
      pricingKeys.page('older-page')
    )?.items[0]?.revision
  ).toBe(1);
});

it('submits modality rates, request fee, batch multiplier, tools, tiers and windows', async () => {
  establish();
  await ready();
  edit('audio-input-price', '4.00');
  edit('cached-audio-input-price', '0.50');
  edit('audio-output-price', '16.00');
  edit('image-input-price', '6.00');
  edit('image-output-price', '24.00');
  edit('per-request-price', '0.01');
  edit('batch-multiplier', '0.50');
  // One tool fee row.
  host
    .querySelectorAll<HTMLButtonElement>('.component-rows button')
    .forEach((button) => {
      if (button.textContent?.includes('Add tool fee')) button.click();
    });
  flushSync();
  edit('tool-name-0', 'web_search');
  edit('tool-fee-0', '0.010');
  // A tier mirrors every per-million rate the entry fills.
  host
    .querySelectorAll<HTMLButtonElement>('.component-rows button')
    .forEach((button) => {
      if (button.textContent?.includes('Add tier')) button.click();
    });
  flushSync();
  edit('tier-threshold-0', '1000000');
  edit('tier-0-input_per_million', '3.00');
  edit('tier-0-audio_input_per_million', '8.00');
  edit('tier-0-cached_audio_input_per_million', '1.00');
  edit('tier-0-audio_output_per_million', '32.00');
  edit('tier-0-image_input_per_million', '12.00');
  edit('tier-0-image_output_per_million', '48.00');
  // A window across every weekday.
  edit('window-zone', 'America/New_York');
  host
    .querySelectorAll<HTMLButtonElement>('.component-rows button')
    .forEach((button) => {
      if (button.textContent?.includes('Add window period')) button.click();
    });
  flushSync();
  host
    .querySelectorAll('.weekday-toggles button')
    .forEach((button) =>
      button.dispatchEvent(new Event('click', { bubbles: true }))
    );
  flushSync();
  edit('window-start-0', '09:00');
  edit('window-end-0', '24:00');
  edit('window-multiplier-0', '0.75');
  submit();
  await vi.waitFor(() => expect(onBusyChange).toHaveBeenLastCalledWith(false));
  const submitted = vi.mocked(createPricingRevision).mock.calls[0]?.[1]?.[0];
  expect(submitted).toMatchObject({
    audio_input_per_million: '4.00',
    cached_audio_input_per_million: '0.50',
    audio_output_per_million: '16.00',
    image_input_per_million: '6.00',
    image_output_per_million: '24.00',
    cached_image_input_per_million: null,
    per_request: '0.01',
    batch_multiplier: '0.50',
    tool_calls: { web_search: '0.010' },
    tiers: [
      {
        above_input_tokens: 1000000,
        input_per_million: '3.00',
        audio_input_per_million: '8.00',
        cached_audio_input_per_million: '1.00',
        audio_output_per_million: '32.00',
        image_input_per_million: '12.00',
        image_output_per_million: '48.00'
      }
    ],
    windows: {
      time_zone: 'America/New_York',
      periods: [
        {
          days: [1, 2, 3, 4, 5, 6, 7],
          start: '09:00',
          end: '24:00',
          multiplier: '0.75'
        }
      ]
    }
  });
});

it('rejects a tool name that cannot price', async () => {
  establish();
  await ready();
  host
    .querySelectorAll<HTMLButtonElement>('.component-rows button')
    .forEach((button) => {
      if (button.textContent?.includes('Add tool fee')) button.click();
    });
  flushSync();
  edit('tool-name-0', 'Web Search');
  edit('tool-fee-0', '0.010');
  submit();
  expect(createPricingRevision).not.toHaveBeenCalled();
  expect(onFeedback).toHaveBeenLastCalledWith({
    status: '',
    error:
      'Tool names are lowercase letters, digits and underscores, starting with a letter.'
  });
});
