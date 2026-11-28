import { useAction } from '#lib/editing/action.svelte.js';
import type { listOidcIdentities } from '#lib/features/identity/profile/api.js';
import {
  startOidcLink,
  unlinkOidc,
  verifyIdentityChange
} from '#lib/features/identity/profile/identityOperations.js';
import { useReauthenticationDialog } from '#lib/features/identity/profile/reauthenticationDialog.svelte.js';
import {
  ENROLLMENT_VERIFIED_MESSAGE,
  type PendingIdentityAction
} from '#lib/features/identity/profile/recentAuthentication.js';

const IDENTITY_UNLINKED =
  'OIDC identity unlinked. All previous sessions were revoked and this browser was rotated.';
const UNLINK_TARGET_MISSING = 'The identity selected for unlinking is missing.';

type OidcIdentities = Awaited<ReturnType<typeof listOidcIdentities>>;

type ProfileIdentityDependencies = {
  identities: () => OidcIdentities | undefined;
  refreshSecurityData: () => Promise<unknown>;
  markEnrollmentVerified: () => void;
  clearMessages: () => void;
  setMessage: (value: string) => void;
};

export function useProfileIdentity(deps: ProfileIdentityDependencies) {
  const identity = useAction();
  let pendingIdentityAction = $state<PendingIdentityAction | undefined>();
  const reauthentication = useReauthenticationDialog(applyChange);

  function resumeSecurityOperation(
    purpose: 'password_enrollment' | 'oidc_link' | 'oidc_unlink',
    resourceId?: string
  ) {
    identity.reset();
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

  function applyChange(change: PendingIdentityAction) {
    if (change.purpose === 'oidc_link') return startOidcLink(identity);
    return unlinkOidc(identity, change.resourceId, async () => {
      pendingIdentityAction = undefined;
      deps.setMessage(IDENTITY_UNLINKED);
      await deps.refreshSecurityData();
    });
  }

  async function completePendingIdentityAction() {
    if (!pendingIdentityAction) return;
    deps.setMessage('');
    await applyChange(pendingIdentityAction);
  }

  async function requestChange(change: PendingIdentityAction) {
    pendingIdentityAction = undefined;
    await verifyIdentityChange(identity, change, {
      usePassword: Boolean(deps.identities()?.has_local_password),
      askForPassword: reauthentication.open
    });
  }

  return {
    identity,
    reauth: reauthentication.action,
    get pendingIdentityAction() {
      return pendingIdentityAction;
    },
    get reauthenticationRequest() {
      return reauthentication.request;
    },
    resumeSecurityOperation,
    completePendingIdentityAction,
    linkIdentity: () => requestChange({ purpose: 'oidc_link' }),
    unlinkIdentity: (resourceId: string) =>
      requestChange({ purpose: 'oidc_unlink', resourceId }),
    confirmWithOidc: reauthentication.confirmWithOidc,
    cancelReauthentication: reauthentication.cancel,
    confirmReauthentication: reauthentication.confirm
  };
}
