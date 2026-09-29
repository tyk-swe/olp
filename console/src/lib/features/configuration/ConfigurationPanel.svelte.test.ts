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
  vi.unstubAllGlobals();
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

it('exports and downloads the configuration artifact', async () => {
  const createObjectURL = vi
    .fn<typeof URL.createObjectURL>()
    .mockReturnValue('blob:configuration');
  const revokeObjectURL = vi.fn<typeof URL.revokeObjectURL>();
  vi.stubGlobal(
    'URL',
    class extends URL {
      static createObjectURL = createObjectURL;
      static revokeObjectURL = revokeObjectURL;
    }
  );
  const download = vi
    .spyOn(HTMLAnchorElement.prototype, 'click')
    .mockImplementation(function (this: HTMLAnchorElement) {
      expect(this.href).toBe('blob:configuration');
      expect(this.download).toBe('openllmproxy-configuration.json');
    });
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
  click('Download JSON');
  expect(download).toHaveBeenCalledOnce();
  expect(createObjectURL).toHaveBeenCalledOnce();
  const blob = createObjectURL.mock.calls[0]![0];
  expect(blob).toBeInstanceOf(Blob);
  expect((blob as Blob).type).toBe('application/json');
  const downloaded = await new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => reject(reader.error);
    reader.readAsText(blob as Blob);
  });
  expect(downloaded).toBe(JSON.stringify(document, null, 2));
  expect(revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:configuration');
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

const digest = 'a1b2c3d4e5f6'.padEnd(64, '0');

const pluginDocument: ConfigurationDocument = {
  ...document,
  providers: [
    {
      name: 'Reference account',
      project: null,
      configuration: {
        kind: 'plugin',
        auth_mode: 'grant',
        profile_id: 'reference-grant-chat',
        profile_revision: digest,
        endpoint: null,
        cloud_region: null,
        cloud_project: null,
        deployment: null,
        api_version: null,
        options: {
          credential_headers: [],
          limits: null,
          models: {},
          parameter_defaults: {},
          vendor_id: null
        }
      },
      models: [],
      slots: []
    }
  ]
};

it('names the plugin build a blocked artifact pins and where to install it', async () => {
  vi.mocked(planConfiguration).mockResolvedValue({
    digest: 'abc123',
    actions: [],
    conflicts: [],
    blockers: [
      {
        kind: 'plugin',
        key: digest,
        action: 'blocker',
        detail: 'plugin_not_installed'
      }
    ]
  });
  render();
  pasteArtifact(JSON.stringify(pluginDocument));
  click('Plan');
  await settle();
  const blockers = host.querySelector('[data-testid="plan-blockers"]')!;
  expect(blockers.textContent?.replace(/\s+/g, ' ')).toContain(
    `Plugin build ${digest} (pinned by Reference account) is not installed here: install and approve it on the Plugins page`
  );
  expect(blockers.querySelector('a')?.getAttribute('href')).toMatch(
    /\/plugins$/
  );
  expect(host.textContent).not.toContain('Apply');
});

it('says an unconfined plugin build needs the deployment to enable its tier', async () => {
  vi.mocked(planConfiguration).mockResolvedValue({
    digest: 'abc123',
    actions: [],
    conflicts: [],
    blockers: [
      {
        kind: 'plugin',
        key: digest,
        action: 'blocker',
        detail: 'plugin_unconfined_disabled'
      }
    ]
  });
  render();
  pasteArtifact(JSON.stringify(pluginDocument));
  click('Plan');
  await settle();
  const blockers = host.querySelector('[data-testid="plan-blockers"]')!;
  expect(blockers.textContent?.replace(/\s+/g, ' ')).toContain(
    `Plugin build ${digest} (pinned by Reference account) is unconfined, and this deployment does not enable unconfined plugins`
  );
  expect(host.textContent).not.toContain('Apply');
});

it('lists the credential slots a grant backs for enrollment after applying', async () => {
  vi.mocked(planConfiguration).mockResolvedValue({
    digest: 'abc123',
    actions: [
      {
        kind: 'provider',
        key: 'Reference account',
        action: 'create',
        detail: 'draft'
      },
      {
        kind: 'credential',
        key: 'Reference account/default',
        action: 'enroll',
        detail: 'grant_enrollment_required'
      }
    ],
    conflicts: [],
    blockers: []
  });
  render();
  pasteArtifact(JSON.stringify(pluginDocument));
  click('Plan');
  await settle();
  const enrollments = host.querySelector(
    '[data-testid="plan-grant-enrollments"]'
  )!;
  expect(enrollments.textContent).toBe('Reference account/default');
  expect(
    host.querySelector('[data-testid="plan-actions"]')?.textContent
  ).not.toContain('Reference account/default');
  expect(
    host.querySelector('#secret-Reference\\ account\\/default')
  ).toBeNull();
  expect(host.textContent).toContain('Apply');
});

it('forgets the secret bindings of an artifact once another replaces it', async () => {
  vi.mocked(planConfiguration).mockResolvedValue(blockedPlan);
  render();
  pasteArtifact(JSON.stringify(document));
  click('Plan');
  await settle();
  const input = host.querySelector<HTMLInputElement>('#secret-acme\\/default')!;
  input.value = 'vendor-secret';
  input.dispatchEvent(new Event('input', { bubbles: true }));

  // OLP refuses a binding the artifact doesn't reference, or one for a slot a
  // grant backs, and the plan it refused would leave no input to clear it.
  pasteArtifact(JSON.stringify(pluginDocument));
  vi.mocked(planConfiguration).mockResolvedValue(cleanPlan);
  click('Plan');
  await settle();
  expect(planConfiguration).toHaveBeenLastCalledWith(pluginDocument, {});
});

it('reports an invalid artifact', async () => {
  render();
  pasteArtifact('{not json');
  expect(host.textContent).toContain('The artifact is not valid JSON.');
});
