import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap } from '$lib/api/http';
import type { Provider } from '$lib/features/providers/api/providers';

type Schemas = components['schemas'];

/** A grant enrollment in progress: the operator signs in upstream at its
 * authorization URL, then continues it with what the upstream returned; or
 * approves its device authorization upstream while the console polls it. */
export type GrantEnrollment = Schemas['GrantEnrollment'];
/** The credential version a completed grant enrollment staged on the draft. */
export type GrantEnrollmentCompletion = Schemas['GrantEnrollmentCompletion'];
/** Where a grant enrollment by device authorization stands. */
export type GrantEnrollmentStatus = Schemas['GrantEnrollmentStatus'];

/** Starts grant enrollment for the provider draft the operator sees, for its
 * default credential slot or, to re-enroll a slot's grant, the slot named. */
export async function startGrantEnrollment(
  provider: Pick<Provider, 'id' | 'etag'>,
  slotId?: string
): Promise<GrantEnrollment> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/grant-enrollments',
    {
      params: {
        path: { provider_id: provider.id },
        header: { 'If-Match': provider.etag }
      },
      body: slotId ? { slot_id: slotId } : undefined
    }
  );
  return unwrap(response);
}

/** Continues a grant enrollment with the pasted callback URL or code. */
export async function continueGrantEnrollment(
  enrollment: Pick<GrantEnrollment, 'id' | 'provider_id'>,
  input: string
): Promise<GrantEnrollmentCompletion> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/continue',
    {
      params: {
        path: {
          provider_id: enrollment.provider_id,
          enrollment_id: enrollment.id
        }
      },
      body: { input }
    }
  );
  return unwrap(response);
}

/** Asks where a grant enrollment by device authorization stands. Once its
 * interval has passed, the request polls the upstream through the plugin. */
export async function pollGrantEnrollment(
  enrollment: Pick<GrantEnrollment, 'id' | 'provider_id'>
): Promise<GrantEnrollmentStatus> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}/poll',
    {
      params: {
        path: {
          provider_id: enrollment.provider_id,
          enrollment_id: enrollment.id
        }
      }
    }
  );
  return unwrap(response);
}

export async function cancelGrantEnrollment(
  enrollment: Pick<GrantEnrollment, 'id' | 'provider_id'>
): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v1/providers/{provider_id}/grant-enrollments/{enrollment_id}',
    {
      params: {
        path: {
          provider_id: enrollment.provider_id,
          enrollment_id: enrollment.id
        }
      }
    }
  );
  ensureOk(response);
}
