import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap } from '$lib/api/http';
type Schemas = components['schemas'];
export type MFAChallenge = Schemas['MFAChallenge'];
export type MFAEnrollment = Schemas['MFAEnrollment'];
export type MFAStatus = Schemas['MFAStatus'];
export type MFAVerification =
  | Schemas['MFAEnrollmentComplete']
  | Schemas['MFABootstrapSession']
  | Schemas['SessionResponse']
  | undefined;
export async function getMFA(signal?: AbortSignal) {
  return unwrap(await apiClient.GET('/api/v1/profile/mfa', { signal }));
}
export async function manageMFA() {
  return unwrap(await apiClient.POST('/api/v1/profile/mfa/challenge'));
}
export async function enrollMFA(
  kind: 'totp' | 'webauthn',
  name: string,
  challenge?: string
) {
  const body = { kind, name, ...(challenge ? { challenge } : {}) };
  return unwrap(
    challenge
      ? await apiClient.POST('/api/v1/auth/mfa/enroll', { body })
      : await apiClient.POST('/api/v1/profile/mfa/enroll', { body })
  );
}
export async function verifyMFA(
  body: Schemas['MFAVerificationRequest'],
  signal?: AbortSignal
): Promise<MFAVerification> {
  const result = await apiClient.POST('/api/v1/auth/mfa/verify', {
    body,
    signal
  });
  ensureOk(result);
  return result.data;
}
export async function removeMFA(id: string, etag: string) {
  ensureOk(
    await apiClient.DELETE('/api/v1/profile/mfa/factors/{factor_id}', {
      params: { path: { factor_id: id }, header: { 'If-Match': `"${etag}"` } }
    })
  );
}
export async function recoveryMFA(etag: string) {
  return unwrap(
    await apiClient.POST('/api/v1/profile/mfa/recovery-codes', {
      params: { header: { 'If-Match': `"${etag}"` } }
    })
  );
}
