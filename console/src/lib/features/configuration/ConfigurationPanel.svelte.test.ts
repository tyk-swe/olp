// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  applyConfiguration,
  exportConfiguration,
  planConfiguration,
  type ConfigurationDocument,
  type ConfigurationPlan
} from '$lib/features/configuration/api';
import ConfigurationPanel from './ConfigurationPanel.svelte';

vi.mock('$lib/features/configuration/api', async (original) => ({
  ...(await original<typeof import('$lib/features/configuration/api')>()),
  applyConfiguration: vi.fn(),
  exportConfiguration: vi.fn(),
  planConfiguration: vi.fn()
}));

const document: ConfigurationDocument = {
  api_version: 'openllmproxy.dev/config/v1',
  projects: [{ name: 'Edge' }],
  providers: [],
  routes: [],
  pricing: null
};

const cleanPlan: ConfigurationPlan = {
  digest: 'abc123',
  actions: [{ kind: 'project', key: 'Edge', action: 'create', detail: '' }],
  conflicts: [],
  blockers: []
};

const blockedPlan: ConfigurationPlan = {
  digest: 'abc123',
  actions: [],
  conflicts: [],
  blockers: [
    {
      kind: 'credential',
      key: 'acme/default',
      action: 'blocker',
      detail: 'secret_binding_required'
    }
  ]
};

let host: HTMLElement;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  host = window.document.createElement('div');
  window.document.body.append(host);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  host.remove();
});

function render() {
  component = mount(ConfigurationPanel, { target: host });
  flushSync();
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function pasteArtifact(value: string) {
  const area = host.querySelector<HTMLTextAreaElement>('#promotion-artifact')!;
  area.value = value;
  area.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function click(text: string) {
  const button = [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === text
  );
  button!.click();
}

it('exports the configuration artifact', async () => {
  vi.mocked(exportConfiguration).mockResolvedValue({
    digest: 'digest-1',
    document
  });
  render();
  click('Export configuration');
  await settle();
  expect(exportConfiguration).toHaveBeenCalled();
  expect(host.textContent).toContain('digest-1');
  expect(host.textContent).toContain('Download JSON');
});

it('plans a pasted artifact and renders actions', async () => {
  vi.mocked(planConfiguration).mockResolvedValue(cleanPlan);
  render();
  pasteArtifact(JSON.stringify(document));
  click('Plan');
  await settle();
  expect(planConfiguration).toHaveBeenCalledWith(document, {});
  expect(host.textContent).toContain('Edge — create');
});

it('collects only the named secret bindings in password inputs', async () => {
  vi.mocked(planConfiguration).mockResolvedValue(blockedPlan);
  render();
  pasteArtifact(JSON.stringify(document));
  click('Plan');
  await settle();
  const input = host.querySelector<HTMLInputElement>('#secret-acme\\/default');
  expect(input?.type).toBe('password');
  expect(host.textContent).not.toContain('Apply');
});

it('applies with the collected bindings', async () => {
  vi.mocked(planConfiguration).mockResolvedValue(blockedPlan);
  vi.mocked(applyConfiguration).mockResolvedValue(cleanPlan);
  render();
  pasteArtifact(JSON.stringify(document));
  click('Plan');
  await settle();
  const input = host.querySelector<HTMLInputElement>('#secret-acme\\/default')!;
  input.value = 'vendor-secret';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  vi.mocked(planConfiguration).mockResolvedValue(cleanPlan);
  click('Plan');
  await settle();
  click('Apply');
  await settle();
  expect(applyConfiguration).toHaveBeenCalledWith(document, {
    'acme/default': 'vendor-secret'
  });
  expect(host.textContent).toContain('Configuration staged.');
});

it('reports an invalid artifact', async () => {
  render();
  pasteArtifact('{not json');
  expect(host.textContent).toContain('The artifact is not valid JSON.');
});
