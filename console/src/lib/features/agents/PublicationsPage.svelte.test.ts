import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import PublicationsProbe from './test/PublicationsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listPublications: vi.fn(),
  listProjects: vi.fn(),
  listServers: vi.fn(),
  getPublication: vi.fn(),
  savePublication: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true, user: null, globalScope: false })
}));

it("saves and reloads a tool's execution classification", async () => {
  const tool = {
    name: 'lookup',
    alias: 'lookup_alias',
    server_id: 'server',
    revision_id: 'revision',
    catalog_digest: 'digest'
  };
  const publication: api.Publication = {
    id: 'toolset',
    project_id: 'project',
    kind: 'toolset',
    name: 'Lookup tools',
    etag: 'etag',
    revision_id: 'revision',
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Lookup tools',
      toolset: { tools: [tool] }
    }
  };
  vi.mocked(api.listPublications).mockResolvedValue([publication]);
  vi.mocked(api.listProjects).mockResolvedValue([]);
  vi.mocked(api.listServers).mockResolvedValue([]);
  vi.mocked(api.getPublication).mockResolvedValue(publication);
  vi.mocked(api.savePublication).mockImplementation(
    async (_kind, current, input) => ({
      ...current!,
      draft: input
    })
  );
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(PublicationsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => expect(host.textContent).toContain('Lookup tools'));
  const open = [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === 'Open'
  );
  expect(open).toBeDefined();
  open!.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Review publication')
  );
  const control = host.querySelector<HTMLSelectElement>(
    'select[aria-label="Execution classification for lookup"]'
  );
  expect(control).not.toBeNull();
  expect(control!.value).toBe('');
  control!.value = 'read_only';
  control!.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const form = host.querySelector<HTMLFormElement>(
    'section[aria-label="Publication editor"] form'
  );
  expect(form).not.toBeNull();
  form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(api.savePublication).toHaveBeenCalled());
  expect(
    vi.mocked(api.savePublication).mock.calls[0][2].toolset?.tools
  ).toEqual([{ ...tool, classification: 'read_only' }]);
  await vi.waitFor(() =>
    expect(
      host.querySelector<HTMLSelectElement>(
        'select[aria-label="Execution classification for lookup"]'
      )?.value
    ).toBe('read_only')
  );
});

it('saves and reloads agent task bounds through the editor', async () => {
  const publication: api.Publication = {
    id: 'agent',
    project_id: 'project',
    kind: 'agent',
    name: 'Bounded agent',
    etag: 'etag',
    revision_id: 'revision',
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Bounded agent',
      agent: {
        protocol: 'a2a-1.0.0',
        endpoint: 'https://agent.example/rpc',
        card_digest: 'digest',
        pricing_revision_id: 'pricing',
        max_cost: '5',
        toolset_revisions: [],
        route_revisions: []
      }
    }
  };
  vi.mocked(api.listPublications).mockImplementation(async (kind) =>
    kind === 'agents' ? [publication] : []
  );
  vi.mocked(api.listProjects).mockResolvedValue([]);
  vi.mocked(api.listServers).mockResolvedValue([]);
  vi.mocked(api.getPublication).mockResolvedValue(publication);
  vi.mocked(api.savePublication).mockClear();
  vi.mocked(api.savePublication).mockImplementation(
    async (_kind, current, input) => ({ ...current!, draft: input })
  );
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(PublicationsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  [...host.querySelectorAll<HTMLButtonElement>('nav button')]
    .find((item) => item.textContent?.trim() === 'agents')!
    .click();
  await vi.waitFor(() => expect(host.textContent).toContain('Bounded agent'));
  [...host.querySelectorAll('button')]
    .find((item) => item.textContent?.trim() === 'Open')!
    .click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Maximum task continuations')
  );
  const input = (label: string) =>
    [...host.querySelectorAll('label')]
      .find((item) => item.textContent?.includes(label))!
      .querySelector('input')!;
  for (const [label, value] of [
    ['Declared maximum remote work duration in seconds', '120'],
    ['Maximum task continuations', '64'],
    ['Task deadline in seconds', '900']
  ]) {
    input(label).value = value;
    input(label).dispatchEvent(new Event('input', { bubbles: true }));
  }
  flushSync();
  host
    .querySelector<HTMLFormElement>(
      'section[aria-label="Publication editor"] form'
    )!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(api.savePublication).toHaveBeenCalled());
  expect(vi.mocked(api.savePublication).mock.calls[0][2].agent).toMatchObject({
    remote_work_bound_seconds: 120,
    max_task_continuations: 64,
    task_deadline_seconds: 900
  });
  await vi.waitFor(() =>
    expect(input('Task deadline in seconds').value).toBe('900')
  );
});

it('converts a legacy prompt, edits its messages and defaults, and saves them', async () => {
  const publication: api.Publication = {
    id: 'prompt',
    project_id: 'project',
    kind: 'prompt',
    name: 'Greeting',
    etag: 'etag',
    revision_id: 'revision',
    retired: false,
    draft: {
      project_id: 'project',
      name: 'Greeting',
      prompt: { template: 'Hello {{name}}', variables: ['name'] }
    }
  };
  vi.mocked(api.listPublications).mockImplementation(async (kind) =>
    kind === 'prompts' ? [publication] : []
  );
  vi.mocked(api.listProjects).mockResolvedValue([]);
  vi.mocked(api.listServers).mockResolvedValue([]);
  vi.mocked(api.getPublication).mockResolvedValue(publication);
  vi.mocked(api.savePublication).mockClear();
  vi.mocked(api.savePublication).mockImplementation(
    async (_kind, current, input) => ({ ...current!, draft: input })
  );
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(PublicationsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  const prompts = [
    ...host.querySelectorAll<HTMLButtonElement>('nav button')
  ].find((item) => item.textContent?.trim() === 'prompts');
  expect(prompts).toBeDefined();
  prompts!.click();
  await vi.waitFor(() => expect(host.textContent).toContain('Greeting'));
  [...host.querySelectorAll('button')]
    .find((item) => item.textContent?.trim() === 'Open')!
    .click();
  await vi.waitFor(() => expect(host.textContent).toContain('Prompt format'));
  const format = [...host.querySelectorAll('label')]
    .find((item) => item.textContent?.includes('Prompt format'))!
    .querySelector('select')!;
  format.value = 'messages';
  format.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const first = [...host.querySelectorAll('fieldset')].find(
    (item) => item.querySelector(':scope > legend')?.textContent === 'Message 1'
  )!;
  expect(first.querySelector('textarea')?.value).toBe('Hello {{name}}');
  first.querySelector('select')!.value = 'system';
  first
    .querySelector('select')!
    .dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  expect(first.querySelector('select')!.value).toBe('system');
  first.querySelector('textarea')!.value = 'Help {{name}}';
  first
    .querySelector('textarea')!
    .dispatchEvent(new Event('input', { bubbles: true }));
  [...host.querySelectorAll('button')]
    .find((item) => item.textContent?.trim() === 'Add message')!
    .click();
  flushSync();
  expect(first.querySelector('select')!.value).toBe('system');
  const second = [...host.querySelectorAll('fieldset')].find(
    (item) => item.querySelector(':scope > legend')?.textContent === 'Message 2'
  )!;
  second.querySelector('textarea')!.value = 'Hello {{name}}';
  second
    .querySelector('textarea')!
    .dispatchEvent(new Event('input', { bubbles: true }));
  const topP = [...host.querySelectorAll('label')]
    .find((item) => item.textContent?.includes('Top P'))!
    .querySelector('input')!;
  topP.value = '0.5';
  topP.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector<HTMLFormElement>(
      'section[aria-label="Publication editor"] form'
    )!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(api.savePublication).toHaveBeenCalled());
  expect(vi.mocked(api.savePublication).mock.calls[0][2].prompt).toEqual({
    messages: [
      { role: 'system', content: 'Help {{name}}' },
      { role: 'user', content: 'Hello {{name}}' }
    ],
    variable_schema: {
      type: 'object',
      properties: { name: { type: 'string' } },
      required: ['name'],
      additionalProperties: false
    },
    default_parameters: { top_p: 0.5 }
  });
});
