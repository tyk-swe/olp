// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import GuardrailsProbe from './test/GuardrailsProbe.svelte';

const fixtures = vi.hoisted(() => ({
  editable: true,
  listDefinitions: vi.fn(),
  listPolicies: vi.fn(),
  listAttachments: vi.fn(),
  listProjects: vi.fn(),
  saveDefinition: vi.fn(),
  savePolicy: vi.fn(),
  activateDefinition: vi.fn(),
  activatePolicy: vi.fn(),
  attachPolicy: vi.fn(),
  evaluatePolicy: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => fixtures.editable })
}));
vi.mock('./api', () => fixtures);
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
const definition = {
  id: 'guard',
  project_id: 'project',
  name: 'PII filter',
  type: 'builtin.pii',
  revision_id: 'revision',
  revision: 1,
  configuration: { categories: ['pii.email'] },
  policy: { rules: [] },
  etag: 'etag',
  active_revision_id: null,
  timeout_ms: 1000,
  on_error: 'fail_closed',
  retired_at: null
};
beforeEach(() => {
  vi.clearAllMocks();
  fixtures.editable = true;
  fixtures.listDefinitions.mockResolvedValue({ items: [definition] });
  fixtures.listPolicies.mockResolvedValue({
    items: [
      {
        id: 'policy',
        policy_id: 'policy',
        name: 'Policy',
        project_id: 'project',
        revision_id: 'policy-revision',
        revision: 1,
        entries: [],
        etag: 'etag'
      }
    ]
  });
  fixtures.listAttachments.mockResolvedValue({ items: [] });
  fixtures.listProjects.mockResolvedValue({
    items: [{ id: 'project', name: 'Project' }]
  });
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
  vi.restoreAllMocks();
});
async function settle() {
  for (let i = 0; i < 3; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}
async function render() {
  component = mount(GuardrailsProbe, { target: host, props: { client } });
  flushSync();
  await settle();
}
function button(text: string) {
  const value = [...host.querySelectorAll('button')].find(
    (node) => node.textContent?.trim() === text
  );
  if (!value) throw new Error(`Missing ${text}`);
  return value;
}
it('labels every editing control and exposes a required egress acknowledgement', async () => {
  await render();
  for (const field of host.querySelectorAll<
    HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement
  >('input,select,textarea'))
    expect(field.labels?.length).toBeGreaterThan(0);
  const type = host.querySelectorAll('select')[1]!;
  type.value = 'webhook';
  type.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const acknowledgement = host.querySelector<HTMLInputElement>(
    'input[type="checkbox"]'
  );
  expect(acknowledgement?.required).toBe(true);
  expect(acknowledgement?.labels?.[0]?.textContent).toContain(
    'sends inspected content'
  );
});
it('evaluates without putting entered content into persistent storage or the query cache', async () => {
  const local = vi.spyOn(Storage.prototype, 'setItem');
  fixtures.evaluatePolicy.mockResolvedValue({
    blocked: false,
    segments: [{ id: 'text', text: 'private example' }],
    decisions: []
  });
  await render();
  button('Playground').click();
  await settle();
  const select = host.querySelector('select')!;
  expect([...select.options].map((option) => option.value)).toContain(
    'policy-revision'
  );
  select.selectedIndex = 1;
  select.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const textarea = host.querySelector('textarea')!;
  textarea.value = 'private example';
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(fixtures.evaluatePolicy).toHaveBeenCalledWith({
    policy_revision_id: 'policy-revision',
    phase: 'input',
    segments: [{ id: 'text', role: 'user', text: 'private example' }]
  });
  expect(local).not.toHaveBeenCalled();
  expect(
    JSON.stringify(
      client
        .getQueryCache()
        .getAll()
        .map((query) => query.state.data)
    )
  ).not.toContain('private example');
  button('Clear content').click();
  flushSync();
  expect(textarea.value).toBe('');
});
it('keeps definition editing unavailable to a read-only member', async () => {
  fixtures.editable = false;
  await render();
  expect(host.textContent).toContain('PII filter');
  expect(host.textContent).not.toContain('Save draft');
});
