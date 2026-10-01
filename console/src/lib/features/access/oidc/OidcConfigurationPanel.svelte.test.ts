import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  beginOidcLink,
  getOidcConfiguration,
  putOidcConfiguration,
  type OidcConfiguration
} from '$lib/features/access/oidc/api';
import {
  listOidcIdentities,
  reauthenticateWithPassword,
  type OidcIdentityList
} from '$lib/features/access/profile/api';
import OidcConfigurationProbe from './test/OidcConfigurationProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/access/oidc/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/oidc/api')>()),
  beginOidcLink: vi.fn(),
  getOidcConfiguration: vi.fn(),
  putOidcConfiguration: vi.fn()
}));
vi.mock('$lib/features/access/profile/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/profile/api')>()),
  listOidcIdentities: vi.fn(),
  reauthenticateWithPassword: vi.fn()
}));

const configuration: OidcConfiguration = {
  id: 'oidc-a',
  etag: 'oidc-v1',
  discovery_url: 'https://identity.example/.well-known/openid-configuration',
  issuer: 'https://identity.example',
  client_id: 'client-a',
  has_client_secret: false,
  enabled: false,
  scopes: ['openid', 'profile', 'email'],
  email_claim: 'email',
  groups_claim: 'groups',
  default_role: 'viewer',
  email_role_mappings: [],
  group_role_mappings: [],
  updated_by_email: 'owner@example.com'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(getOidcConfiguration).mockResolvedValue(configuration);
  dialogPrototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
  if (originalShowModal)
    Object.defineProperty(
      HTMLDialogElement.prototype,
      'showModal',
      originalShowModal
    );
  else delete dialogPrototype.showModal;
  vi.unstubAllGlobals();
});

const dialogPrototype = HTMLDialogElement.prototype as unknown as Record<
  string,
  unknown
>;
const originalShowModal = Object.getOwnPropertyDescriptor(
  HTMLDialogElement.prototype,
  'showModal'
);

function field(id: string) {
  const result = host.querySelector<HTMLInputElement>(`#${id}`);
  if (!result) throw new Error(`Missing OIDC field: ${id}`);
  return result;
}

function edit(id: string, value: string) {
  const input = field(id);
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

it('keeps edits made during validation dirty for the next save', async () => {
  const pending = Promise.withResolvers<OidcConfiguration>();
  vi.mocked(putOidcConfiguration)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce({
      ...configuration,
      etag: 'oidc-v3',
      issuer: 'https://newer.example',
      has_client_secret: true
    });
  component = mount(OidcConfigurationProbe, {
    target: host,
    props: { client }
  });
  await vi.waitFor(() =>
    expect(field('oidc-issuer').value).toBe(configuration.issuer)
  );

  edit('oidc-issuer', 'https://submitted.example');
  edit('client-secret', 'submitted-secret');
  submit();
  await vi.waitFor(() => expect(putOidcConfiguration).toHaveBeenCalledTimes(1));

  edit('oidc-issuer', 'https://newer.example');
  edit('client-secret', 'newer-secret');
  pending.resolve({
    ...configuration,
    etag: 'oidc-v2',
    issuer: 'https://submitted.example',
    has_client_secret: true
  });
  await vi.waitFor(() => {
    flushSync();
    expect(field('oidc-issuer').value).toBe('https://newer.example');
    expect(field('client-secret').value).toBe('newer-secret');
    expect(host.textContent).toContain('additional unsaved changes');
  });

  submit();
  await vi.waitFor(() => expect(putOidcConfiguration).toHaveBeenCalledTimes(2));
  expect(putOidcConfiguration).toHaveBeenNthCalledWith(
    2,
    expect.objectContaining({
      issuer: 'https://newer.example',
      client_secret: 'newer-secret'
    }),
    'oidc-v2'
  );
});

async function confirmLinkWithPassword() {
  vi.mocked(listOidcIdentities).mockResolvedValue({
    has_local_password: true
  } as OidcIdentityList);
  vi.mocked(getOidcConfiguration).mockResolvedValue({
    ...configuration,
    enabled: true
  });
  const assign = vi.fn();
  vi.stubGlobal('location', { ...window.location, assign });
  component = mount(OidcConfigurationProbe, {
    target: host,
    props: { client }
  });
  await vi.waitFor(() =>
    expect(field('oidc-issuer').value).toBe(configuration.issuer)
  );
  [...host.querySelectorAll('button')]
    .find((button) => button.textContent?.trim() === 'Link my identity')!
    .click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('#reauth-password')).toBeTruthy();
  });
  edit('reauth-password', 'correct horse');
  host
    .querySelector('dialog form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  return assign;
}

it('shows an OIDC link failure on the panel after the password is accepted', async () => {
  vi.mocked(reauthenticateWithPassword).mockResolvedValue(undefined as never);
  vi.mocked(beginOidcLink).mockRejectedValue(new Error('OIDC is unavailable'));
  const assign = await confirmLinkWithPassword();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('dialog')).toBeNull();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'OIDC is unavailable'
    );
  });
  expect(assign).not.toHaveBeenCalled();
});

it('keeps the dialog open when the password is rejected', async () => {
  vi.mocked(reauthenticateWithPassword).mockRejectedValue(
    new Error('Incorrect password')
  );
  await confirmLinkWithPassword();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('dialog')?.textContent).toContain(
      'Incorrect password'
    );
  });
  expect(beginOidcLink).not.toHaveBeenCalled();
});
