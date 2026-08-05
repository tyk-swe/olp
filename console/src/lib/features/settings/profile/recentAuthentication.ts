import type { RecentAuthenticationPurpose } from '$lib/api/operations';

export const ENROLLMENT_GRANT_TTL_MS = 5 * 60 * 1000;
export const ENROLLMENT_GRANT_READY_MESSAGE =
  'Identity verified. Add your local password within five minutes.';

export type PendingIdentityAction =
  { purpose: 'oidc_link' } | { purpose: 'oidc_unlink'; resourceId: string };

export type RecentAuthenticationCallback = {
  purpose: RecentAuthenticationPurpose;
  resourceId?: string;
};

export function parseRecentAuthenticationCallback(
  search: string
): RecentAuthenticationCallback | null {
  const parameters = new URLSearchParams(search);
  const purpose = parameters.get('reauthenticated');
  if (
    purpose !== 'password_enrollment' &&
    purpose !== 'oidc_link' &&
    purpose !== 'oidc_unlink'
  ) {
    return null;
  }
  return {
    purpose,
    resourceId: parameters.get('resource_id') ?? undefined
  };
}
