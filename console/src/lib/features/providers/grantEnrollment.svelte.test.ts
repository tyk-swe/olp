import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { copyText } from '$lib/clipboard';
import { projectKeys } from '$lib/features/access/projects/projectKeys';
import { providerKeys } from './providerKeys';
import {
  createProvider,
  probeProvider,
  updateProvider,
  type Provider,
  type ProviderProbe
} from './api/providers';
import {
  cancelGrantEnrollment,
  continueGrantEnrollment,
  pollGrantEnrollment,
  startGrantEnrollment,
  type GrantEnrollment,
  type GrantEnrollmentCompletion
} from './api/grants';
import {
  listProviderCredentials,
  type ProviderCredential
} from './api/credentials';
import {
  declareProviderModels,
  listProviderModelPage,
  type ProviderKindCapability
} from './api/models';
import type { ProviderProfile } from './api/profiles';
import PluginProviderProbe from './test/PluginProviderProbe.svelte';
import { pluginSpec, referenceProfile } from './test/pluginFixtures';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/clipboard', () => ({ copyText: vi.fn() }));
vi.mock('./api/providers', async (original) => ({
  ...(await original<typeof import('./api/providers')>()),
  createProvider: vi.fn(),
  updateProvider: vi.fn(),
  probeProvider: vi.fn()
}));
vi.mock('./api/grants', () => ({
  startGrantEnrollment: vi.fn(),
  continueGrantEnrollment: vi.fn(),
  pollGrantEnrollment: vi.fn(),
  cancelGrantEnrollment: vi.fn()
}));
vi.mock('./api/credentials', async (original) => ({
  ...(await original<typeof import('./api/credentials')>()),
  listProviderCredentials: vi.fn()
}));
vi.mock('./api/models', async (original) => ({
  ...(await original<typeof import('./api/models')>()),
  listProviderModelPage: vi.fn(),
  declareProviderModels: vi.fn()
}));

const digest = 'e'.repeat(64);
const authorizationURL =
  'https://login.example.com/authorize?client_id=olp-reference&state=s1&code_challenge=c&code_challenge_method=S256';

const probeSpec: ProviderKindCapability = {
  ...pluginSpec,
  fields: [{ field: 'model', label: 'Probe model', required: true }]
};

const referenceGrantChat = referenceProfile(digest, {
  id: 'reference-grant-chat',
  label: 'Reference Chat Completions with sign-in',
  authentication: ['grant']
});

const saved: Provider = {
  id: 'provider-account',
  name: 'Reference account',
  project_id: null,
  project_name: null,
  configuration: {
    kind: 'plugin',
    auth_mode: 'grant',
    profile_id: referenceGrantChat.id,
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
  state: 'draft',
  connector_ready: true,
  pending_activation: true,
  etag: 'v1',
  created_at: '2026-09-27T06:00:00Z',
  updated_at: '2026-09-27T06:00:00Z',
  model_count: 1,
  enabled_model_count: 1,
  capability_count: 2,
  certified_capability_count: 0,
  draft_credential_id: null
};
const enrolled: Provider = {
  ...saved,
  etag: 'v2',
  draft_credential_id: 'credential-1',
  draft_credential_version: 1
};
const completion: GrantEnrollmentCompletion = {
  provider_id: saved.id,
  etag: 'v2',
  credential_id: 'credential-1',
  credential_version: 1,
  principal: 'operator@reference.example'
};

/** The credential version a completed grant enrollment staged on the draft. */
const grantVersion: ProviderCredential = {
  id: 'credential-1',
  version: 1,
  active: false,
  draft_selected: true,
  created_at: '2026-09-27T06:05:00Z',
  revoked_at: null,
  grant: {
    plugin_digest: digest,
    principal: 'operator@reference.example',
    facts: {},
    expires_at: '2026-09-27T07:00:00Z',
    lapsed_at: null
  }
};

const enrollment: GrantEnrollment = {
  id: 'enrollment-1',
  provider_id: saved.id,
  slot_id: 'slot-default',
  authorization_url: authorizationURL,
  expires_at: '2026-09-27T06:10:00Z'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let provider: Provider;

beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify({ title: 'Not Found', status: 404 }), {
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
  client.setQueryData(providerKeys.kinds(), [probeSpec]);
  client.setQueryData(providerKeys.vendors(), []);
  client.setQueryData(providerKeys.configurationSchemas(), {});
  client.setQueryData(projectKeys.memberships, []);
  client.setQueryData(providerKeys.profiles(), [referenceGrantChat]);
  provider = saved;
  vi.mocked(createProvider).mockResolvedValue(saved.id);
  vi.mocked(updateProvider).mockImplementation(async () => provider);
  vi.mocked(listProviderModelPage).mockImplementation(async () => ({
    provider,
    items: [],
    nextCursor: null
  }));
  vi.mocked(startGrantEnrollment).mockResolvedValue(enrollment);
  vi.mocked(listProviderCredentials).mockResolvedValue([grantVersion]);
  vi.mocked(probeProvider).mockResolvedValue({
    succeeded: true,
    detail: 'Reached the upstream; 1 models listed.',
    probe_type: 'models',
    discovered_models: 1
  } as ProviderProbe);
});

afterEach(async () => {
  vi.useRealTimers();
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

async function settle() {
  for (let i = 0; i < 3; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function set(selector: string, value: string, event = 'input') {
  const element = host.querySelector<HTMLInputElement | HTMLSelectElement>(
    selector
  )!;
  if (element instanceof HTMLInputElement && element.type === 'radio')
    element.checked = true;
  else element.value = value;
  element.dispatchEvent(new Event(event, { bubbles: true }));
  flushSync();
}

function submit(form: Element) {
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

/** Fills the Connection stage for one of the reference plugin's profiles, its
 * grant profile by default. */
async function connectionStage(profileId = 'reference-grant-chat') {
  component = mount(PluginProviderProbe, { target: host, props: { client } });
  flushSync();
  await settle();
  set('input[name="kind"][value="plugin"]', 'plugin', 'change');
  set('#provider-plugin-profile', `${profileId}@${digest}`, 'change');
  set('#provider-name', 'Reference account');
  set('#initial-model', 'reference-model');
}

function panel() {
  return host.querySelector('[aria-labelledby="grant-enrollment-heading"]');
}

describe('grant enrollment in the provider wizard', () => {
  it('replaces the credential with a sign-in upstream, then continues to discovery', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    vi.mocked(continueGrantEnrollment).mockImplementation(async () => {
      provider = enrolled;
      return completion;
    });
    await connectionStage();

    expect(host.querySelector<HTMLSelectElement>('#provider-auth')?.value).toBe(
      'grant'
    );
    expect(host.querySelector('#provider-secret')).toBeNull();
    expect(host.textContent).toContain('Grant enrollment');
    const save = host.querySelector<HTMLButtonElement>(
      'form button[type="submit"]'
    )!;
    expect(save.textContent).toContain('Save and sign in upstream');
    submit(host.querySelector('form')!);
    await vi.waitFor(() => expect(startGrantEnrollment).toHaveBeenCalled());
    const [input] = vi.mocked(createProvider).mock.calls[0]!;
    expect(input.configuration.auth_mode).toBe('grant');
    expect(input.credential).toBeUndefined();
    expect(vi.mocked(startGrantEnrollment).mock.calls[0]![0]).toMatchObject({
      id: saved.id,
      etag: 'v1'
    });
    // The connection is tested only once a grant backs it.
    expect(probeProvider).not.toHaveBeenCalled();

    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });
    const link = panel()!.querySelector<HTMLAnchorElement>('a')!;
    expect(link.href).toBe(authorizationURL);
    expect(link.target).toBe('_blank');
    expect(link.rel).toContain('noopener');
    panel()!
      .querySelector<HTMLButtonElement>(
        'button[aria-label="Copy authorization URL"]'
      )!
      .click();
    await settle();
    expect(copyText).toHaveBeenCalledWith(authorizationURL);
    expect(panel()!.textContent).toContain('Copied');

    const callback = 'http://127.0.0.1:1455/callback?code=signed-in&state=s1';
    set('#grant-input', ` ${callback} `);
    submit(panel()!.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
    expect(continueGrantEnrollment).toHaveBeenCalledWith(enrollment, callback);
    expect(vi.mocked(probeProvider).mock.calls[0]![0].etag).toBe('v2');
    expect(host.textContent).not.toContain('signed-in');
  });

  it('shows why a continuation failed and signs in again from the start', async () => {
    vi.mocked(continueGrantEnrollment).mockRejectedValue(
      new ApiProblem({
        type: 'https://openllmproxy.dev/problems/grant_state_mismatch',
        title: 'Unprocessable Entity',
        status: 422,
        detail:
          'What was pasted back answers another sign-in than this grant enrollment’s.'
      })
    );
    await connectionStage();
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });

    submit(panel()!.querySelector('form')!);
    await settle();
    expect(continueGrantEnrollment).not.toHaveBeenCalled();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'Paste the callback URL'
    );

    set('#grant-input', 'code#another-state');
    submit(panel()!.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'answers another sign-in'
      );
    });
    // A grant enrollment is continued once, so the operator signs in again.
    expect(panel()).toBeNull();
    expect(probeProvider).not.toHaveBeenCalled();
    submit(host.querySelector('form')!);
    await vi.waitFor(() =>
      expect(startGrantEnrollment).toHaveBeenCalledTimes(2)
    );
    expect(updateProvider).toHaveBeenCalledOnce();
  });

  it('cancels a sign-in the operator abandons', async () => {
    vi.mocked(cancelGrantEnrollment).mockResolvedValue();
    await connectionStage();
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });
    [...panel()!.querySelectorAll('button')]
      .find((button) => button.textContent?.includes('Cancel sign-in'))!
      .click();
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).toBeNull();
    });
    expect(cancelGrantEnrollment).toHaveBeenCalledWith(enrollment);
  });
});

function saveButton() {
  return host.querySelector<HTMLButtonElement>('form button[type="submit"]')!;
}

/** Enrolls the draft's grant, which takes the wizard to discovery, then
 * returns to the Connection stage. */
async function enrollThenReturn() {
  vi.mocked(continueGrantEnrollment).mockImplementation(async () => {
    provider = enrolled;
    return completion;
  });
  await connectionStage();
  submit(host.querySelector('form')!);
  await vi.waitFor(() => {
    flushSync();
    expect(panel()).not.toBeNull();
  });
  set('#grant-input', 'code#s1');
  submit(panel()!.querySelector('form')!);
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
  [...host.querySelectorAll('button')]
    .find((button) => button.textContent?.trim() === 'Back')!
    .click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('#provider-plugin-profile')).not.toBeNull();
  });
}

describe('saving the Connection stage of a draft that holds a credential', () => {
  it('tests the connection when a live grant of the pinned build backs the draft', async () => {
    await enrollThenReturn();
    await vi.waitFor(() => {
      flushSync();
      expect(saveButton().textContent).toContain('Save and test connection');
    });
    submit(host.querySelector('form')!);
    await vi.waitFor(() => expect(probeProvider).toHaveBeenCalledTimes(2));
    expect(startGrantEnrollment).toHaveBeenCalledOnce();
  });

  it.each([
    [
      'lapsed',
      {
        ...grantVersion,
        grant: { ...grantVersion.grant!, lapsed_at: '2026-09-27T06:45:00Z' }
      }
    ],
    ['was revoked', { ...grantVersion, revoked_at: '2026-09-27T06:45:00Z' }]
  ])(
    "signs in upstream again once the draft's grant %s",
    async (_, version) => {
      await enrollThenReturn();
      vi.mocked(listProviderCredentials).mockResolvedValue([version]);
      submit(host.querySelector('form')!);
      await vi.waitFor(() => {
        flushSync();
        expect(panel()).not.toBeNull();
      });
      expect(startGrantEnrollment).toHaveBeenCalledTimes(2);
      expect(probeProvider).toHaveBeenCalledOnce();
    }
  );

  it('signs in upstream again once the draft moves to another build of the plugin', async () => {
    const nextDigest = 'f'.repeat(64);
    client.setQueryData(providerKeys.profiles(), [
      referenceGrantChat,
      {
        ...referenceGrantChat,
        revision: nextDigest,
        plugin: { digest: nextDigest, name: 'reference', version: '0.2.0' }
      }
    ]);
    await enrollThenReturn();
    set(
      '#provider-plugin-profile',
      `reference-grant-chat@${nextDigest}`,
      'change'
    );
    expect(saveButton().textContent).toContain('Save and sign in upstream');
    vi.mocked(updateProvider).mockImplementation(async () => {
      provider = {
        ...enrolled,
        etag: 'v3',
        configuration: {
          ...enrolled.configuration,
          profile_revision: nextDigest
        }
      };
      return provider;
    });
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });
    expect(startGrantEnrollment).toHaveBeenCalledTimes(2);
    expect(vi.mocked(startGrantEnrollment).mock.calls[1]![0]).toMatchObject({
      etag: 'v3'
    });
    expect(probeProvider).toHaveBeenCalledOnce();
  });

  it('signs in upstream once the operator moves a draft holding a static credential to a grant profile', async () => {
    const referenceChat: ProviderProfile = {
      ...referenceGrantChat,
      id: 'reference-chat',
      label: 'Reference Chat Completions',
      authentication: ['static_credential']
    };
    client.setQueryData(providerKeys.profiles(), [
      referenceChat,
      referenceGrantChat
    ]);
    provider = {
      ...enrolled,
      configuration: {
        ...saved.configuration,
        auth_mode: 'static_credential',
        profile_id: referenceChat.id
      }
    };
    vi.mocked(listProviderCredentials).mockResolvedValue([
      { ...grantVersion, grant: null }
    ]);
    await connectionStage('reference-chat');
    set('#provider-secret', 'static-secret');
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
    [...host.querySelectorAll('button')]
      .find((button) => button.textContent?.trim() === 'Back')!
      .click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('#provider-plugin-profile')).not.toBeNull();
    });

    set('#provider-plugin-profile', `reference-grant-chat@${digest}`, 'change');
    expect(saveButton().textContent).toContain('Save and sign in upstream');
    vi.mocked(updateProvider).mockImplementation(async () => {
      provider = { ...enrolled, etag: 'v3' };
      return provider;
    });
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(panel()).not.toBeNull();
    });
    expect(startGrantEnrollment).toHaveBeenCalledOnce();
    expect(probeProvider).toHaveBeenCalledOnce();
  });
});

const verificationURL = 'https://login.example.com/device';
const deviceEnrollment: GrantEnrollment = {
  id: 'enrollment-device',
  provider_id: saved.id,
  slot_id: 'slot-default',
  device: {
    verification_url: verificationURL,
    user_code: 'WDJB-MJHT',
    interval: 5
  },
  expires_at: '2026-09-27T06:15:00Z'
};

/** Saves the Connection stage for a profile whose plugin starts a device
 * authorization, with timers faked from then on so the test decides when each
 * interval passes, and returns once the panel shows it. */
async function deviceAuthorization() {
  vi.mocked(startGrantEnrollment).mockResolvedValue(deviceEnrollment);
  await connectionStage();
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  submit(host.querySelector('form')!);
  for (let i = 0; i < 20 && !panel(); i++) {
    await vi.advanceTimersByTimeAsync(0);
    flushSync();
  }
  expect(panel()).not.toBeNull();
}

describe('grant enrollment by device authorization', () => {
  it('shows the verification URL and user code, polls each interval and continues on approval', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    vi.mocked(pollGrantEnrollment)
      .mockResolvedValueOnce({ status: 'pending', interval: 10 })
      .mockImplementationOnce(async () => {
        provider = enrolled;
        return { status: 'completed', completion };
      });
    await deviceAuthorization();

    const link = panel()!.querySelector<HTMLAnchorElement>('a')!;
    expect(link.href).toBe(verificationURL);
    expect(link.textContent).toContain('Open verification page');
    expect(link.target).toBe('_blank');
    expect(panel()!.querySelector('.user-code')?.textContent).toBe('WDJB-MJHT');
    expect(panel()!.querySelector('#grant-input')).toBeNull();
    for (const [label, value] of [
      ['Copy verification URL', verificationURL],
      ['Copy user code', 'WDJB-MJHT']
    ]) {
      const button = panel()!.querySelector<HTMLButtonElement>(
        `button[aria-label="${label}"]`
      )!;
      button.click();
      await vi.advanceTimersByTimeAsync(0);
      flushSync();
      expect(copyText).toHaveBeenLastCalledWith(value);
      expect(button.textContent).toBe('Copied');
    }
    expect(panel()!.querySelector('[role="status"]')?.textContent).toContain(
      'Waiting for approval upstream'
    );

    // Each status request waits the interval OLP last reported.
    await vi.advanceTimersByTimeAsync(4999);
    expect(pollGrantEnrollment).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(pollGrantEnrollment).toHaveBeenCalledOnce();
    expect(pollGrantEnrollment).toHaveBeenCalledWith(deviceEnrollment);
    await vi.advanceTimersByTimeAsync(9999);
    expect(pollGrantEnrollment).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1);
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
    vi.useRealTimers();

    // Approved upstream, the grant backs the draft, and the connection is
    // tested with it.
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
    expect(panel()).toBeNull();
    expect(vi.mocked(probeProvider).mock.calls[0]![0].etag).toBe('v2');
  });

  it('stops polling once the operator denies the device, and signs in again from the start', async () => {
    vi.mocked(pollGrantEnrollment).mockResolvedValue({ status: 'denied' });
    await deviceAuthorization();
    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'denied upstream'
    );
    expect(panel()).toBeNull();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pollGrantEnrollment).toHaveBeenCalledOnce();
    expect(probeProvider).not.toHaveBeenCalled();
    vi.useRealTimers();
    submit(host.querySelector('form')!);
    await vi.waitFor(() =>
      expect(startGrantEnrollment).toHaveBeenCalledTimes(2)
    );
  });

  it('asks again when OLP could not answer, and stops once a poll fails', async () => {
    vi.mocked(pollGrantEnrollment)
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockRejectedValueOnce(
        new ApiProblem({
          type: 'https://openllmproxy.dev/problems/grant_enrollment_failed',
          title: 'Unprocessable Entity',
          status: 422,
          detail: 'The plugin could not enroll a grant (http_failed).'
        })
      );
    await deviceAuthorization();
    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(panel()).not.toBeNull();
    expect(host.querySelector('[role="alert"]')).toBeNull();
    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'could not enroll a grant'
    );
    expect(panel()).toBeNull();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pollGrantEnrollment).toHaveBeenCalledTimes(2);
  });

  it('reports a device authorization that expired', async () => {
    vi.mocked(pollGrantEnrollment).mockResolvedValue({ status: 'expired' });
    await deviceAuthorization();
    await vi.advanceTimersByTimeAsync(5000);
    flushSync();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'expired before it was approved'
    );
    expect(panel()).toBeNull();
  });

  it('stops polling a device authorization the operator cancels', async () => {
    vi.mocked(cancelGrantEnrollment).mockResolvedValue();
    await deviceAuthorization();
    [...panel()!.querySelectorAll('button')]
      .find((button) => button.textContent?.includes('Cancel sign-in'))!
      .click();
    await vi.advanceTimersByTimeAsync(0);
    flushSync();
    expect(panel()).toBeNull();
    expect(cancelGrantEnrollment).toHaveBeenCalledWith(deviceEnrollment);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(pollGrantEnrollment).not.toHaveBeenCalled();
  });
});

describe('stepping back in the provider wizard', () => {
  it('re-saves the stored configuration, keeping model facts recorded after creation', async () => {
    const referenceChat: ProviderProfile = {
      ...referenceGrantChat,
      id: 'reference-chat',
      authentication: ['static_credential']
    };
    client.setQueryData(providerKeys.profiles(), [referenceChat]);
    provider = {
      ...saved,
      configuration: {
        ...saved.configuration,
        auth_mode: 'static_credential',
        profile_id: referenceChat.id
      }
    };
    // Upstream facts discovery recorded; only their survival matters here.
    const models = {
      'reference-model': { context_length: 32768 }
    } as unknown as NonNullable<Provider['configuration']['options']>['models'];
    vi.mocked(declareProviderModels).mockImplementation(async () => {
      provider = {
        ...provider,
        etag: 'v2',
        configuration: {
          ...provider.configuration,
          options: { ...provider.configuration.options!, models }
        }
      };
      return provider as never;
    });
    await connectionStage('reference-chat');
    set('#provider-secret', 'static-secret');
    submit(host.querySelector('form')!);
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
    set('#manual-models-wizard', 'reference-model');
    [...host.querySelectorAll('button')]
      .find(
        (button) => button.textContent?.trim() === 'Add identifiers for review'
      )!
      .click();
    await vi.waitFor(() => expect(declareProviderModels).toHaveBeenCalled());
    await settle();
    [...host.querySelectorAll('button')]
      .find((button) => button.textContent?.trim() === 'Back')!
      .click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('#provider-plugin-profile')).not.toBeNull();
    });
    submit(host.querySelector('form')!);
    await vi.waitFor(() => expect(updateProvider).toHaveBeenCalled());
    const [id, etag, input] = vi.mocked(updateProvider).mock.calls[0]!;
    expect(id).toBe(saved.id);
    expect(etag).toBe('v2');
    // Numbers keep their exact JSON text, so compare the serialized document.
    expect(JSON.stringify(input.configuration?.options?.models)).toBe(
      JSON.stringify(models)
    );
  });
});
