// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
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
  await authLifecycle.signOut(async () => {});
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
  expect(applyConfiguration).toHaveBeenCalledWith(
    document,
    {
      'acme/default': 'vendor-secret'
    },
    expect.any(String),
    undefined,
    undefined
  );
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
          commitment: null,
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

async function planWithSecret() {
  vi.mocked(planConfiguration).mockResolvedValue(blockedPlan);
  click('Plan');
  await settle();
  const input = host.querySelector<HTMLInputElement>('#secret-acme\\/default')!;
  input.value = 'vendor-secret';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  vi.mocked(planConfiguration).mockResolvedValue(cleanPlan);
  click('Plan');
  await settle();
}

it('does not replace a newer pasted artifact with an earlier file read', async () => {
  let readFile!: (text: string) => void;
  const pendingFile = new Promise<string>((resolve) => (readFile = resolve));
  render();
  pasteArtifact(JSON.stringify(document));
  await planWithSecret();
  const fileInput = host.querySelector<HTMLInputElement>('input[type="file"]')!;
  Object.defineProperty(fileInput, 'files', {
    value: [{ text: () => pendingFile }]
  });
  fileInput.dispatchEvent(new Event('change', { bubbles: true }));
  await settle();
  const current = JSON.stringify({
    ...document,
    projects: [{ name: 'Current' }]
  });
  pasteArtifact(current);
  readFile(JSON.stringify(document));
  await settle();
  expect(
    host.querySelector<HTMLTextAreaElement>('#promotion-artifact')!.value
  ).toBe(current);
  expect(host.querySelector('#secret-acme\\/default')).toBeNull();
  click('Plan');
  await settle();
  expect(planConfiguration).toHaveBeenLastCalledWith(JSON.parse(current), {});
});

it.each(['unmount', 'principal replacement'])(
  'does not re-send secret bindings after a delayed conflict and %s',
  async (boundary) => {
    let rejectApply!: (reason: unknown) => void;
    vi.mocked(applyConfiguration).mockImplementationOnce(
      () => new Promise((_, reject) => (rejectApply = reject))
    );
    render();
    pasteArtifact(JSON.stringify(document));
    await planWithSecret();
    click('Apply');
    await settle();
    expect(applyConfiguration).toHaveBeenCalledTimes(1);
    const plans = vi.mocked(planConfiguration).mock.calls.length;

    if (boundary === 'unmount') {
      await unmount(component!);
      component = undefined;
    } else {
      authLifecycle.establishSession({
        user: {
          id: 'replacement-operator',
          email: 'replacement@example.com',
          display_name: 'Replacement',
          role: 'operator',
          access_scope: 'global',
          operations: []
        },
        csrf_token: 'replacement-csrf'
      });
      await settle();
    }
    rejectApply(
      new ApiProblem({ title: 'Stale configuration plan', status: 409 })
    );
    await settle();
    expect(planConfiguration).toHaveBeenCalledTimes(plans);
    expect(applyConfiguration).toHaveBeenCalledTimes(1);
    if (boundary === 'principal replacement') {
      expect(
        host.querySelector<HTMLTextAreaElement>('#promotion-artifact')!.value
      ).toBe('');
      expect(host.querySelector('#secret-acme\\/default')).toBeNull();
    }
  }
);

it('keeps an unchanged ambiguous apply retry on its original submission', async () => {
  vi.mocked(applyConfiguration)
    .mockRejectedValueOnce(new Error('The response was lost.'))
    .mockResolvedValueOnce(cleanPlan);
  render();
  pasteArtifact(JSON.stringify(document));
  await planWithSecret();
  click('Apply');
  await settle();
  click('Apply');
  await settle();
  const calls = vi.mocked(applyConfiguration).mock.calls;
  expect(calls).toHaveLength(2);
  expect(calls[1]).toEqual(calls[0]);
  expect(host.textContent).toContain('Configuration staged.');
});

it.each([
  { name: 'valid replacement', artifact: JSON.stringify(pluginDocument) },
  { name: 'invalid replacement', artifact: '{not json' }
])(
  'retires an ambiguous secret-binding submission after $name',
  async ({ artifact: replacement }) => {
    vi.mocked(applyConfiguration)
      .mockRejectedValueOnce(new Error('The response was lost.'))
      .mockResolvedValueOnce(cleanPlan);
    render();
    pasteArtifact(JSON.stringify(document));
    await planWithSecret();
    click('Apply');
    await settle();
    const original = vi.mocked(applyConfiguration).mock.calls[0]!;

    pasteArtifact(replacement);
    pasteArtifact(JSON.stringify(document));
    await planWithSecret();
    click('Apply');
    await settle();
    const resumed = vi.mocked(applyConfiguration).mock.calls[1]!;
    expect(resumed[0]).toEqual(original[0]);
    expect(resumed[1]).toEqual(original[1]);
    expect(resumed[2]).not.toBe(original[2]);
    expect(host.textContent).toContain('Configuration staged.');
  }
);

it('reports an invalid artifact', async () => {
  render();
  pasteArtifact('{not json');
  expect(host.textContent).toContain('The artifact is not valid JSON.');
});

it('ignores a plan that answers an artifact replaced meanwhile', async () => {
  const pending = Promise.withResolvers<ConfigurationPlan>();
  vi.mocked(planConfiguration).mockReturnValueOnce(pending.promise);
  render();
  pasteArtifact(JSON.stringify(document));
  click('Plan');
  flushSync();
  expect(
    host.querySelector<HTMLTextAreaElement>('#promotion-artifact')!.readOnly
  ).toBe(true);
  pasteArtifact(JSON.stringify(pluginDocument));
  pending.resolve(cleanPlan);
  await settle();
  expect(host.querySelector('[data-testid="promotion-plan"]')).toBeNull();
  expect(
    [...host.querySelectorAll('button')].some(
      (button) => button.textContent?.trim() === 'Apply'
    )
  ).toBe(false);
  vi.mocked(planConfiguration).mockResolvedValue(cleanPlan);
  click('Plan');
  await settle();
  expect(planConfiguration).toHaveBeenLastCalledWith(pluginDocument, {});
  expect(host.textContent).toContain('Apply');
});
