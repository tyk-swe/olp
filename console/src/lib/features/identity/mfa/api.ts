import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  unwrapOutcome,
  ifMatch
} from '#lib/management/http.js';
import {
  parseMFAChallenge,
  type MFAVerification
} from '#lib/session/mfaChallenge.js';
type Schemas = components['schemas'];
export type MFAFactor = Schemas['MFAStatus']['factors'][number];
export type MFAKind = Schemas['MFAEnrollmentRequest']['kind'];
export const mfaKindLabels: Record<MFAKind, string> = {
  totp: 'Authenticator app',
  webauthn: 'Security key or passkey'
};
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
  kind: MFAKind,
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
      params: { path: { factor_id: id }, header: ifMatch(etag) }
    })
  );
}
export async function recoveryMFA(etag: string) {
  return unwrap(
    await apiClient.POST('/api/v1/profile/mfa/recovery-codes', {
      params: { header: ifMatch(etag) }
    })
  );
}
