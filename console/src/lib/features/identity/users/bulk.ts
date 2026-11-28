import { errorMessage } from '#lib/management/http.js';
import { updateUser, type UserPatch } from './api';
import { type User } from '#lib/domain/users/api.js';

/** Each member retains its observed ETag and the ordinary API's safety checks.
 * Sequential requests keep partial outcomes clear if authority changes mid-run. */
export async function updateMembers(members: User[], patch: UserPatch) {
  const updated: User[] = [];
  const failed: { user: User; message: string }[] = [];
  for (const user of members) {
    try {
      updated.push(await updateUser(user, patch));
    } catch (cause) {
      failed.push({ user, message: errorMessage(cause) });
    }
  }
  return { updated, failed };
}
