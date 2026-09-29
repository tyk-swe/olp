import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type Invitation = Schemas['InvitationResponse'];
export type InvitationSecret = Schemas['CreateInvitationResponse'];

export async function listInvitationPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Invitation>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/invitations', {
      params: { query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function createInvitation(
  email: string,
  role: string,
  expiresInHours?: number
): Promise<InvitationSecret> {
  return unwrap(
    await apiClient.POST('/api/v1/invitations', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      // Omitting the field lets the API apply its seven-day default; it caps
      // the lifetime at thirty days (720 hours).
      body: {
        email,
        role,
        ...(expiresInHours ? { expires_in_hours: expiresInHours } : {})
      }
    })
  );
}

export async function revokeInvitation(id: string): Promise<void> {
  unwrap(
    await apiClient.DELETE('/api/v1/invitations/{invitation_id}', {
      params: {
        path: { invitation_id: id },
        header: { 'Idempotency-Key': crypto.randomUUID() }
      }
    })
  );
}
