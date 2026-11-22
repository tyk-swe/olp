import { useAction } from '#lib/editing/action.svelte.js';
import { errorMessage } from '#lib/management/http.js';
import { beginOidcLink } from '#lib/features/access/oidc/api.js';
import {
  beginIdentityReauthentication,
  listOidcIdentities,
  unlinkOidcIdentity
} from '#lib/features/access/profile/api.js';
import { reauthenticateWithPassword } from '#lib/features/access/profile/reauthentication.js';
import {
  ENROLLMENT_VERIFIED_MESSAGE,
  type PendingIdentityAction
} from '#lib/features/access/profile/recentAuthentication.js';

export const SECURITY_OPERATION_FAILED =
  'The security operation could not be completed.';
export const IDENTITY_UNLINKED =
  'OIDC identity unlinked. All previous sessions were revoked and this browser was rotated.';
export const UNLINK_TARGET_MISSING =
  'The identity selected for unlinking is missing.';

type OidcIdentities = Awaited<ReturnType<typeof listOidcIdentities>>;

export type ReauthenticationRequest = {
  purpose: 'oidc_link' | 'oidc_unlink';
  resourceId?: string;
};

export function useProfileIdentity(deps: {
  identities: () => OidcIdentities | undefined;
  refreshSecurityData: () => Promise<unknown>;
  markEnrollmentVerified: () => void;
  clearMessages: () => void;
  setMessage: (value: string) => void;
}) {
  const identity = useAction();
  const reauth = useAction();
  let pendingIdentityAction = $state<PendingIdentityAction | undefined>();
  let reauthenticationRequest = $state<ReauthenticationRequest | null>(null);

  function resumeSecurityOperation(
    purpose: 'password_enrollment' | 'oidc_link' | 'oidc_unlink',
    resourceId?: string
  ) {
    identity.error = '';
    deps.clearMessages();
    if (purpose === 'password_enrollment') {
      pendingIdentityAction = undefined;
      deps.markEnrollmentVerified();
      deps.setMessage(ENROLLMENT_VERIFIED_MESSAGE);
      return;
    }
    if (purpose === 'oidc_link') {
      pendingIdentityAction = { purpose };
      deps.setMessage(
        'Identity verified. Confirm before linking your OIDC identity.'
      );
      return;
    }
    if (!resourceId) {
      identity.error = UNLINK_TARGET_MISSING;
      return;
    }
    pendingIdentityAction = { purpose, resourceId };
    deps.setMessage(
      'Identity verified. Confirm before unlinking this OIDC identity.'
    );
  }

  async function completePendingIdentityAction() {
    const action = pendingIdentityAction;
    if (!action) return;
    deps.setMessage('');
    await identity.run(
      async () => {
        if (action.purpose === 'oidc_link')
          return { redirect: await beginOidcLink() };
        await unlinkOidcIdentity(action.resourceId);
        return { unlinked: true as const };
      },
      async (outcome) => {
        if ('redirect' in outcome) {
          if (outcome.redirect !== undefined)
            window.location.assign(outcome.redirect);
          return;
        }
        pendingIdentityAction = undefined;
        deps.setMessage(IDENTITY_UNLINKED);
        await deps.refreshSecurityData();
      },
      {
        key: action.purpose === 'oidc_link' ? 'link' : action.resourceId,
        failure: (cause) => errorMessage(cause, SECURITY_OPERATION_FAILED)
      }
    );
  }

  async function linkIdentity() {
    pendingIdentityAction = undefined;
    await identity.run(
      async () => {
        if (deps.identities()?.has_local_password)
          return { reauth: true as const };
        return {
          redirect: await beginIdentityReauthentication('oidc_link')
        };
      },
      (outcome) => {
        if ('reauth' in outcome) {
          reauth.error = '';
          reauthenticationRequest = { purpose: 'oidc_link' };
          return;
        }
        if (outcome.redirect !== undefined)
          window.location.assign(outcome.redirect);
      },
      {
        key: 'link',
        failure: (cause) =>
          errorMessage(cause, 'The OIDC link flow could not start.')
      }
    );
  }

  async function unlinkIdentity(id: string) {
    pendingIdentityAction = undefined;
    await identity.run(
      async () => {
        if (deps.identities()?.has_local_password)
          return { reauth: true as const };
        return {
          redirect: await beginIdentityReauthentication('oidc_unlink', id)
        };
      },
      (outcome) => {
        if ('reauth' in outcome) {
          reauth.error = '';
          reauthenticationRequest = { purpose: 'oidc_unlink', resourceId: id };
          return;
        }
        if (outcome.redirect !== undefined)
          window.location.assign(outcome.redirect);
      },
      {
        key: id,
        failure: (cause) =>
          errorMessage(cause, 'The OIDC identity could not be unlinked.')
      }
    );
  }

  async function confirmWithOidc() {
    const request = reauthenticationRequest;
    if (!request) return;
    await reauth.run(
      () => beginIdentityReauthentication(request.purpose, request.resourceId),
      (url) => window.location.assign(url),
      {
        failure: (cause) => errorMessage(cause, SECURITY_OPERATION_FAILED)
      }
    );
  }

  function cancelReauthentication() {
    reauthenticationRequest = null;
    reauth.error = '';
  }

  async function confirmReauthentication(password: string) {
    const request = reauthenticationRequest;
    if (!request) return;
    await reauth.run(
      () =>
        reauthenticateWithPassword(
          password,
          request.purpose,
          request.resourceId
        ),
      async () => {
        reauthenticationRequest = null;
        if (request.purpose === 'oidc_link') {
          await identity.run(
            () => beginOidcLink(),
            (url) => window.location.assign(url),
            {
              key: 'link',
              failure: (cause) => errorMessage(cause, SECURITY_OPERATION_FAILED)
            }
          );
          return;
        }
        const resourceId = request.resourceId;
        if (!resourceId) {
          identity.error = UNLINK_TARGET_MISSING;
          return;
        }
        await identity.run(
          () => unlinkOidcIdentity(resourceId),
          async () => {
            deps.setMessage(IDENTITY_UNLINKED);
            await deps.refreshSecurityData();
          },
          {
            key: resourceId,
            failure: (cause) => errorMessage(cause, SECURITY_OPERATION_FAILED)
          }
        );
      },
      {
        failure: (cause) => errorMessage(cause, SECURITY_OPERATION_FAILED)
      }
    );
  }

  return {
    identity,
    reauth,
    get pendingIdentityAction() {
      return pendingIdentityAction;
    },
    get reauthenticationRequest() {
      return reauthenticationRequest;
    },
    resumeSecurityOperation,
    completePendingIdentityAction,
    linkIdentity,
    unlinkIdentity,
    confirmWithOidc,
    cancelReauthentication,
    confirmReauthentication
  };
}
