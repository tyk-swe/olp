import type { useAction } from '#lib/editing/action.svelte.js';
import { errorMessage } from '#lib/management/http.js';
import { beginOidcLink } from '#lib/features/identity/oidc/api.js';
import {
  beginIdentityReauthentication,
  unlinkOidcIdentity
} from '#lib/features/identity/profile/api.js';
import type { PendingIdentityAction } from '#lib/features/identity/profile/recentAuthentication.js';

type Action = Pick<ReturnType<typeof useAction>, 'run'>;

export function securityFailure(cause: unknown) {
  return errorMessage(cause, 'The security operation could not be completed.');
}

export function startOidcLink(action: Action) {
  return action.run(beginOidcLink, (url) => window.location.assign(url), {
    key: 'link',
    failure: securityFailure
  });
}

export function unlinkOidc(
  action: Action,
  identityId: string,
  unlinked: () => Promise<void>
) {
  return action.run(() => unlinkOidcIdentity(identityId), unlinked, {
    key: identityId,
    failure: securityFailure
  });
}

async function nextVerificationStep(
  change: PendingIdentityAction,
  usePassword: boolean
) {
  if (usePassword) return { kind: 'password' as const };
  return {
    kind: 'redirect' as const,
    url: await beginIdentityReauthentication(change.purpose, change.resourceId)
  };
}

export function verifyIdentityChange(
  action: Action,
  change: PendingIdentityAction,
  {
    usePassword,
    askForPassword
  }: {
    usePassword: boolean;
    askForPassword: (change: PendingIdentityAction) => void;
  }
) {
  const fallback =
    change.purpose === 'oidc_link'
      ? 'The OIDC link flow could not start.'
      : 'The OIDC identity could not be unlinked.';
  return action.run(
    () => nextVerificationStep(change, usePassword),
    (step) => {
      if (step.kind === 'password') askForPassword(change);
      else window.location.assign(step.url);
    },
    {
      key: change.resourceId ?? 'link',
      failure: (cause) => errorMessage(cause, fallback)
    }
  );
}
