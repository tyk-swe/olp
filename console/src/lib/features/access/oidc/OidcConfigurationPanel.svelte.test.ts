import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  getOidcConfiguration,
  putOidcConfiguration,
  type OidcConfiguration
} from '$lib/features/access/oidc/api';
import OidcConfigurationProbe from './test/OidcConfigurationProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/access/oidc/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/oidc/api')>()),
  getOidcConfiguration: vi.fn(),
  putOidcConfiguration: vi.fn()
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
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
});

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
