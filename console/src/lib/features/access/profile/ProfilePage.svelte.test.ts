import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  getProfile,
  listOidcIdentities,
  updateProfile,
  type UserProfile
} from '$lib/features/access/profile/api';
import { listSessionPage } from '$lib/features/access/api';
import { userKeys } from '$lib/features/access/users/userKeys';
import ProfilePageProbe from './test/ProfilePageProbe.svelte';

vi.mock('$app/navigation', () => ({ replaceState: vi.fn() }));
vi.mock('$lib/features/access/session/lifecycle', () => ({
  authLifecycle: {
    validateSession: vi.fn().mockResolvedValue(undefined),
    endCurrentSession: vi.fn()
  }
}));
vi.mock('$lib/features/access/profile/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/profile/api')>()),
  getProfile: vi.fn(),
  listOidcIdentities: vi.fn(),
  updateProfile: vi.fn(),
  beginOidcReauthentication: vi.fn()
}));
vi.mock('$lib/features/access/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api')>()),
  listSessionPage: vi.fn()
}));

const profile: UserProfile = {
  id: 'user-a',
  email: 'owner@example.com',
  display_name: 'Original owner',
  role: 'owner',
  active: true,
  etag: 'profile-v1',
  created_at: '2026-09-15T12:00:00Z',
  updated_at: '2026-09-15T12:00:00Z'
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
  vi.mocked(getProfile).mockResolvedValue(profile);
  vi.mocked(listOidcIdentities).mockResolvedValue({
    has_local_password: true,
    items: [],
    linking_available: true
  });
  vi.mocked(listSessionPage).mockResolvedValue({
    items: [],
    nextCursor: null
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
});

function nameField() {
  const result = host.querySelector<HTMLInputElement>('#profile-name');
  if (!result) throw new Error('Missing profile name input');
  return result;
}

function editName(value: string) {
  const input = nameField();
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function saveProfile() {
  nameField()
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

it('keeps a newer display name dirty after an earlier save completes', async () => {
  const pending = Promise.withResolvers<UserProfile>();
  vi.mocked(updateProfile)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce({
      ...profile,
      display_name: 'Newest owner',
      etag: 'profile-v3'
    });
  component = mount(ProfilePageProbe, {
    target: host,
    props: { client }
  });
  client.setQueryData(userKeys.roster, [profile]);
  await vi.waitFor(() => expect(nameField().value).toBe(profile.display_name));

  editName('Submitted owner');
  saveProfile();
  await vi.waitFor(() => expect(updateProfile).toHaveBeenCalledTimes(1));

  editName('Newest owner');
  pending.resolve({
    ...profile,
    display_name: 'Submitted owner',
    etag: 'profile-v2'
  });
  await vi.waitFor(() => {
    flushSync();
    expect(nameField().value).toBe('Newest owner');
    expect(host.textContent).toContain('additional unsaved changes');
    expect(client.getQueryState(userKeys.roster)?.isInvalidated).toBe(true);
  });

  saveProfile();
  await vi.waitFor(() => expect(updateProfile).toHaveBeenCalledTimes(2));
  expect(updateProfile).toHaveBeenNthCalledWith(
    2,
    expect.objectContaining({ etag: 'profile-v2' }),
    { display_name: 'Newest owner' }
  );
});

it('offers linked OIDC reauthentication alongside an enrolled password with the same purpose and resource', async () => {
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const { beginOidcReauthentication } =
    await import('$lib/features/access/profile/api');
  vi.mocked(beginOidcReauthentication).mockRejectedValue(
    new Error('Fresh provider flow unavailable')
  );
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', {
    configurable: true,
    value: vi.fn()
  });
  vi.mocked(listOidcIdentities).mockResolvedValue({
    has_local_password: true,
    linking_available: true,
    oidc_reauthentication_available: true,
    items: [
      {
        id: 'identity-a',
        issuer: 'https://issuer.test',
        email_at_link: 'owner@example.com',
        created_at: profile.created_at,
        last_login_at: null,
        can_unlink: true
      }
    ]
  });
  component = mount(ProfilePageProbe, { target: host, props: { client } });
  await vi.waitFor(() => expect(host.textContent).toContain('Unlink'));
  [...host.querySelectorAll('button')]
    .find((button) => button.textContent?.trim() === 'Unlink')!
    .click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Verify with single sign-on')
  );
  expect(host.querySelector('dialog input[type="password"]')).not.toBeNull();
  [...host.querySelectorAll('button')]
    .find(
      (button) => button.textContent?.trim() === 'Verify with single sign-on'
    )!
    .click();
  await vi.waitFor(() =>
    expect(beginOidcReauthentication).toHaveBeenCalledWith(
      'oidc_unlink',
      'identity-a'
    )
  );
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Fresh provider flow unavailable')
  );
});
