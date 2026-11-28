import { useAction } from '#lib/editing/action.svelte.js';
import { beginIdentityReauthentication } from '#lib/features/identity/profile/api.js';
import { securityFailure } from '#lib/features/identity/profile/identityOperations.js';
import { reauthenticateWithPassword } from '#lib/features/identity/profile/reauthentication.js';
import type { PendingIdentityAction } from '#lib/features/identity/profile/recentAuthentication.js';

export function useReauthenticationDialog(
  verified: (change: PendingIdentityAction) => Promise<unknown>
) {
  const action = useAction();
  let request = $state<PendingIdentityAction | null>(null);

  return {
    action,
    get request() {
      return request;
    },
    open(change: PendingIdentityAction) {
      action.reset();
      request = change;
    },
    cancel() {
      request = null;
      action.reset();
    },
    async confirmWithOidc() {
      const change = request;
      if (!change) return;
      await action.run(
        () => beginIdentityReauthentication(change.purpose, change.resourceId),
        (url) => window.location.assign(url),
        { failure: securityFailure }
      );
    },
    async confirm(password: string) {
      const change = request;
      if (!change) return;
      await action.run(
        () =>
          reauthenticateWithPassword(
            password,
            change.purpose,
            change.resourceId
          ),
        async () => {
          request = null;
          await verified(change);
        },
        { failure: securityFailure }
      );
    }
  };
}
