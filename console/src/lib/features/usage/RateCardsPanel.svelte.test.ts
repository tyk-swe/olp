import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import { listOrganizations } from '$lib/features/access/organizations/api';
import { listProjects } from '$lib/features/access/projects/api';
import { listProviderKinds } from '$lib/features/providers/api/models';
import { listProviderVendors } from '$lib/features/providers/api/providers';
import {
  createRateCard,
  createRateCardRevision,
  listRateCards,
  type RateCard
} from './api/rateCards';
import RateCardsProbe from './test/RateCardsProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('$lib/features/access/organizations/api', () => ({
  listOrganizations: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjects: vi.fn()
}));
vi.mock('$lib/features/providers/api/models', () => ({
  listProviderKinds: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviderVendors: vi.fn()
}));
vi.mock('./api/rateCards', () => ({
  createRateCard: vi.fn(),
  createRateCardRevision: vi.fn(),
  listRateCards: vi.fn()
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
const card: RateCard = {
  id: 'card-1',
  owner: { kind: 'project', project: 'Billed', project_id: 'project-1' },
  revisions: [
    {
      id: 'rev-1',
      rate_card_id: 'card-1',
      revision: 1,
      markup: '1.25',
      effective_at: '2026-09-30T00:00:00Z',
      created_at: '2026-09-30T12:00:00Z',
      created_by: 'owner'
    }
  ],
  created_at: '2026-09-29T00:00:00Z'
};
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

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
  vi.mocked(listOrganizations).mockResolvedValue([
    {
      id: 'org-1',
      name: 'Charge org',
      created_at: '',
      budget_policy: null
    } as never
  ]);
  vi.mocked(listProjects).mockResolvedValue([
    { id: 'project-1', name: 'Billed' } as never
  ]);
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
  vi.mocked(listRateCards).mockResolvedValue({
    items: [card],
    nextCursor: null
  });
  vi.mocked(createRateCard).mockResolvedValue(card);
  vi.mocked(createRateCardRevision).mockResolvedValue({
    ...card.revisions![0],
    prices: []
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function establish() {
  component = mount(RateCardsProbe, { target: host, props: { client } });
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

function submit(form: HTMLFormElement) {
  form.dispatchEvent(
    new SubmitEvent('submit', { bubbles: true, cancelable: true })
  );
  flushSync();
}

it('lists cards and creates an installation card', async () => {
  establish();
  await vi.waitFor(() => expect(host.textContent).toContain('Project Billed'));
  const form = host.querySelector<HTMLFormElement>('form')!;
  submit(form);
  await vi.waitFor(() =>
    expect(vi.mocked(createRateCard)).toHaveBeenCalledWith({
      kind: 'installation'
    })
  );
});

it('creates a project card for the chosen owner', async () => {
  establish();
  await vi.waitFor(() => expect(field('card-owner-kind')).toBeTruthy());
  edit('card-owner-kind', 'project');
  edit('card-project', 'project-1');
  submit(host.querySelector<HTMLFormElement>('form')!);
  await vi.waitFor(() =>
    expect(vi.mocked(createRateCard)).toHaveBeenCalledWith({
      kind: 'project',
      id: 'project-1'
    })
  );
});

it('publishes a revision with markup and an explicit price', async () => {
  establish();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Publish revision')
  );
  const buttons = [...host.querySelectorAll('button')].filter(
    (b) => b.textContent === 'Publish revision'
  );
  buttons[0].dispatchEvent(new Event('click', { bubbles: true }));
  flushSync();
  await vi.waitFor(() => expect(field('markup-card-1')).toBeTruthy());
  edit('markup-card-1', '1.5');
  // Add an explicit price row and fill its scope + rate.
  const add = [...host.querySelectorAll('button')].find(
    (b) => b.textContent === 'Add explicit price'
  )!;
  add.dispatchEvent(new Event('click', { bubbles: true }));
  flushSync();
  await vi.waitFor(() => expect(field('card-kind-0')).toBeTruthy());
  edit('card-kind-0', 'openai');
  edit('card-model-0', 'model-a');
  edit('card-0-input-price', ' 5.000 ');
  const form = host.querySelector<HTMLFormElement>('.revision-form')!;
  submit(form);
  await vi.waitFor(() =>
    expect(vi.mocked(createRateCardRevision)).toHaveBeenCalled()
  );
  const [cardId, draft] = vi.mocked(createRateCardRevision).mock.calls[0];
  expect(cardId).toBe('card-1');
  expect(draft.markup).toBe('1.5');
  expect(draft.effective_at).toBeUndefined();
  expect(draft.prices?.[0].currency).toBeUndefined();
  expect(draft.prices?.[0]).toMatchObject({
    provider_kind: 'openai',
    model: 'model-a',
    input_per_million: '5.000'
  });
});
