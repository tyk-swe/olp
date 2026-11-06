import type { components } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import {
  ApiProblem,
  ensureOk,
  unwrap,
  unwrapOutcome,
  type ApiOutcome
} from '$lib/management/http';
type Schemas = components['schemas'];
export type MFAChallenge = Schemas['MFAChallenge'];
export type MFAEnrollment = Schemas['MFAEnrollment'];
export type MFAStatus = Schemas['MFAStatus'];
export type MFAVerification =
  | Schemas['MFAEnrollmentComplete']
  | Schemas['MFABootstrapSession']
  | Schemas['SessionResponse']
  | undefined;
export function parseMFAChallenge(value: unknown): ApiOutcome<MFAChallenge> {
  if (
    typeof value === 'object' &&
    value !== null &&
    'challenge' in value &&
    typeof value.challenge === 'string' &&
    'expires_at' in value &&
    typeof value.expires_at === 'string' &&
    'enrollment_required' in value &&
    typeof value.enrollment_required === 'boolean' &&
    'methods' in value &&
    Array.isArray(value.methods) &&
    value.methods.every(
      (method: unknown) =>
        method === 'totp' || method === 'webauthn' || method === 'recovery'
    ) &&
    'public_key' in value &&
    (value.public_key === null ||
      (typeof value.public_key === 'object' &&
        !Array.isArray(value.public_key))) &&
    (!('webauthn_available' in value) ||
      value.webauthn_available === undefined ||
      typeof value.webauthn_available === 'boolean')
  ) {
    return { kind: 'success', value: value as MFAChallenge };
  }
  return {
    kind: 'failure',
    problem: new ApiProblem({
      type: 'urn:olp:problem:invalid-mfa-response',
      title: 'The API response did not include a valid MFA challenge',
      status: 502
    })
  };
}

export async function getMFA(signal?: AbortSignal) {
  return unwrap(await apiClient.GET('/api/v1/profile/mfa', { signal }));
}
export async function manageMFA() {
  return unwrapOutcome(
    parseMFAChallenge(
      unwrap(await apiClient.POST('/api/v1/profile/mfa/challenge'))
    )
  );
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
