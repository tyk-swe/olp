import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { providerKeys } from './providerKeys';
import type { Provider } from './api';
import {
  listProviderCredentials,
  type ProviderCredential
} from './credentials';
import {
  continueGrantEnrollment,
  startGrantEnrollment,
  type GrantEnrollment
} from './grants';
import { listProviderModelPage, type ProviderKindCapability } from './models';
import PluginProviderProbe from './test/PluginProviderProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./grants', () => ({
  startGrantEnrollment: vi.fn(),
  continueGrantEnrollment: vi.fn(),
  cancelGrantEnrollment: vi.fn()
}));
vi.mock('./credentials', async (original) => ({
  ...(await original<typeof import('./credentials')>()),
  listProviderCredentials: vi.fn()
}));
vi.mock('./models', async (original) => ({
  ...(await original<typeof import('./models')>()),
  listProviderModelPage: vi.fn()
}));

const digest = 'e'.repeat(64);
const principal = 'operator@reference.example';

const pluginSpec: ProviderKindCapability = {
  kind: 'plugin',
  label: 'Provider plugin',
  description: 'A profile an installed provider plugin supplies.',
  default_auth_mode: 'static_credential',
  auth_modes: [
    {
      mode: 'static_credential',
      label: 'Static credential',
      credential: 'required'
    },
    { mode: 'grant', label: 'Grant', credential: 'grant' }
  ],
  fields: [],
  presets: []
};

const provider: Provider = {
  id: 'provider-account',
  name: 'Reference account',
  project_id: null,
  project_name: null,
  configuration: {
    kind: 'plugin',
    auth_mode: 'grant',
    profile_id: 'reference-grant-chat',
    profile_revision: digest,
    endpoint: 'https://api.example.com/v1',
    options: {
      vendor_id: null,
      limits: null,
      credential_headers: [],
      parameter_defaults: {},
      models: {}
    }
  },
  state: 'active',
  connector_ready: true,
  pending_activation: false,
  active_revision: 1,
  etag: 'v3',
  created_at: '2026-09-27T06:00:00Z',
  updated_at: '2026-09-27T06:00:00Z',
  model_count: 1,
  enabled_model_count: 1,
  capability_count: 2,
  certified_capability_count: 2,
  draft_credential_id: 'credential-1',
  draft_credential_version: 1
};

const grant = {
  plugin_digest: digest,
  principal,
  facts: { account: 'acct-reference' },
  expires_at: '2026-09-27T07:00:00Z'
};
const firstVersion: ProviderCredential = {
  id: 'credential-1',
  version: 1,
  active: true,
  draft_selected: true,
  created_at: '2026-09-27T06:00:00Z',
  revoked_at: null,
  grant
};

const enrollment: GrantEnrollment = {
  id: 'enrollment-1',
  provider_id: provider.id,
  slot_id: 'slot-default',
  authorization_url:
    'https://login.example.com/authorize?client_id=olp-reference&state=s1',
  expires_at: '2026-09-27T06:10:00Z'
};

const slots = {
  etag: 'slots-v1',
  connection_usage: null,
  items: [
    {
      id: 'slot-default',
      name: 'default',
      enabled: true,
      priority: 0,
      weight: 1,
      credential_version_id: 'credential-1',
      allowed_models: [],
      allowed_routes: [],
      allowed_api_keys: [],
      requests_per_minute: null,
      tokens_per_minute: null,
      max_concurrency: null
    },
    {
      id: 'slot-standby',
      name: 'Standby',
      enabled: true,
      priority: 1,
      weight: 1,
      credential_version_id: null,
      allowed_models: [],
      allowed_routes: [],
      allowed_api_keys: [],
      requests_per_minute: null,
      tokens_per_minute: null,
      max_concurrency: null
    }
  ],
  health: {
    'slot-default': {
      revoked: false,
      active_credential_version_id: 'credential-1',
      cooling_down: null,
      validated_at: '2026-09-27T06:01:00Z',
      usage: null
    },
    'slot-standby': { revoked: false, active_credential_version_id: null }
  }
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal(
    'fetch',
    vi.fn(async (request: Request) =>
      new URL(request.url).pathname.endsWith('/credential-slots')
        ? new Response(JSON.stringify(slots), {
            headers: { 'Content-Type': 'application/json' }
          })
        : new Response(JSON.stringify({ title: 'Not Found', status: 404 }), {
            status: 404,
            headers: { 'Content-Type': 'application/problem+json' }
          })
    )
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(providerKeys.kinds(), [pluginSpec]);
  client.setQueryData(['provider-profiles'], []);
  vi.mocked(listProviderModelPage).mockResolvedValue({
    provider,
    items: [],
    nextCursor: null
  });
  vi.mocked(listProviderCredentials).mockResolvedValue([firstVersion]);
  vi.mocked(startGrantEnrollment).mockResolvedValue(enrollment);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

function set(selector: string, value: string) {
  const element = host.querySelector<HTMLInputElement>(selector)!;
  element.value = value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function pool() {
  return host.querySelector('[aria-labelledby="credential-pool-heading"]')!;
}

function versions() {
  return host.querySelector('[aria-labelledby="credential-heading"]')!;
}

function panel() {
  return host.querySelector('[aria-labelledby="grant-enrollment-heading"]');
}

function button(within: Element, label: string) {
  return [...within.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === label
  );
}

/** Opens the provider page, once its credential pool lists the slots. */
async function openProvider() {
  component = mount(PluginProviderProbe, {
    target: host,
    props: { client, providerId: provider.id }
  });
  await vi.waitFor(() => {
    flushSync();
    expect(button(pool(), 'Re-enroll grant')).toBeDefined();
  });
}

/** Opens the provider and signs in again for its default slot. */
async function reenrollDefaultSlot() {
  await openProvider();
  button(pool(), 'Re-enroll grant')!.click();
  await vi.waitFor(() => {
    flushSync();
    expect(panel()).not.toBeNull();
  });
}

describe('grant re-enrollment in the credential pool', () => {
  it('stages a new credential version and shows the observed principal on credential versions', async () => {
    vi.mocked(continueGrantEnrollment).mockImplementation(async () => {
      vi.mocked(listProviderCredentials).mockResolvedValue([
        {
          ...firstVersion,
          id: 'credential-2',
          version: 2,
          active: false,
          created_at: '2026-09-27T06:30:00Z'
        },
        { ...firstVersion, draft_selected: false }
      ]);
      return {
        provider_id: provider.id,
        etag: 'v4',
        credential_id: 'credential-2',
        credential_version: 2,
        principal
      };
    });
    await reenrollDefaultSlot();
    expect(versions().textContent).toContain(`Observed principal ${principal}`);
    expect(startGrantEnrollment).toHaveBeenCalledWith(
      expect.objectContaining({ id: provider.id, etag: 'v3' }),
      'slot-default'
    );
    // Other slots can't start another sign-in meanwhile.
    expect(button(pool(), 'Re-enroll grant')!.disabled).toBe(true);

    const callback = 'http://127.0.0.1:1455/callback?code=signed-in&state=s1';
    set('#grant-input', ` ${callback} `);
    panel()!
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => {
      flushSync();
      expect(pool().querySelector('[role="status"]')?.textContent).toContain(
        `default: grant enrolled for ${principal} as credential version 2, pending activation.`
      );
    });
    expect(continueGrantEnrollment).toHaveBeenCalledWith(enrollment, callback);
    expect(panel()).toBeNull();
    await vi.waitFor(() => {
      flushSync();
      expect(versions().textContent).toContain('Version 2');
    });
    expect(versions().textContent).toContain('pending activation');
    expect(host.textContent).not.toContain('signed-in');
  });

  it('edits a slot without a pasted credential and shows why a re-enrollment failed', async () => {
    vi.mocked(continueGrantEnrollment).mockRejectedValue(
      new ApiProblem({
        type: 'https://openllmproxy.dev/problems/grant_enrollment_failed',
        title: 'Unprocessable Entity',
        status: 422,
        detail:
          'The plugin could not enroll a grant (invalid_grant): the code expired.'
      })
    );
    await reenrollDefaultSlot();
    set('#grant-input', 'code#s1');
    panel()!
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => {
      flushSync();
      expect(pool().querySelector('[role="alert"]')?.textContent).toContain(
        'the code expired'
      );
    });
    // A grant enrollment is continued once: the operator signs in again.
    expect(panel()).toBeNull();
    expect(button(pool(), 'Re-enroll grant')!.disabled).toBe(false);

    button(pool(), 'Edit')!.click();
    flushSync();
    expect(pool().querySelector('form')).not.toBeNull();
    expect(pool().querySelector('input[type="password"]')).toBeNull();
  });

  it('enrolls the first grant of a slot without a credential version, such as an imported one', async () => {
    await openProvider();
    button(pool(), 'Enroll grant')!.click();
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });
    expect(startGrantEnrollment).toHaveBeenCalledWith(
      expect.objectContaining({ id: provider.id }),
      'slot-standby'
    );
  });
});
