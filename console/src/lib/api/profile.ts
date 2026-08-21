import type { components } from './schema';
import { apiClient } from './client';
import { ensureSuccess, result } from './http';

export type UserProfile = components['schemas']['UserDetailResponse'];
export type OidcIdentityList =
  components['schemas']['OidcIdentityListResponse'];

export type ProfileUpdate = { display_name: string };
export type PasswordChange = { current_password: string; new_password: string };
export type PasswordEnrollment = { new_password: string };
export type RecentAuthenticationPurpose =
  'password_enrollment' | 'oidc_link' | 'oidc_unlink';

export async function getProfile(): Promise<UserProfile> {
  const { data, error, response } = await apiClient.GET('/api/v1/profile');
  return result(data, error, response);
}

export async function updateProfile(
  profile: UserProfile,
  input: ProfileUpdate
): Promise<UserProfile> {
  const { data, error, response } = await apiClient.PATCH('/api/v1/profile', {
    params: { header: { 'If-Match': profile.etag } },
    body: input
  });
  return result(data, error, response);
}

export async function reauthenticateWithPassword(
  currentPassword: string,
  purpose: RecentAuthenticationPurpose,
  resourceId?: string
): Promise<void> {
  const { error, response } = await apiClient.POST(
    '/api/v1/profile/reauthenticate',
    {
      body: {
        current_password: currentPassword,
        purpose,
        ...(resourceId ? { resource_id: resourceId } : {})
      }
    }
  );
  ensureSuccess(error, response);
}

export async function changePassword(
  profile: UserProfile,
  input: PasswordChange
): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/profile/password',
    {
      params: { header: { 'If-Match': profile.etag } },
      body: input
    }
  );
  return result(data, error, response);
}

export async function enrollPassword(
  profile: UserProfile,
  input: PasswordEnrollment
): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/profile/password/enroll',
    {
      params: { header: { 'If-Match': profile.etag } },
      body: input
    }
  );
  return result(data, error, response);
}

export async function listOidcIdentities(): Promise<OidcIdentityList> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/oidc/identities'
  );
  return result(data, error, response);
}

export async function beginOidcReauthentication(
  purpose: RecentAuthenticationPurpose,
  resourceId?: string
): Promise<string> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/oidc/reauthenticate',
    { body: { purpose, ...(resourceId ? { resource_id: resourceId } : {}) } }
  );
  return result(data, error, response).authorization_url;
}

export async function unlinkOidcIdentity(identityId: string): Promise<void> {
  const { error, response } = await apiClient.DELETE(
    '/api/v1/oidc/identities/{identity_id}',
    { params: { path: { identity_id: identityId } } }
  );
  ensureSuccess(error, response);
}
