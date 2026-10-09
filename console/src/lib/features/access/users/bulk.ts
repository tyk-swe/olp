import { errorMessage } from '$lib/api/http';
import { updateUser, type User, type UserPatch } from './api';

/** Each member retains its observed ETag and the ordinary API's safety checks.
 * Sequential requests keep partial outcomes clear if authority changes mid-run. */
export async function updateMembers(
  members: User[],
  patch: UserPatch,
  update: typeof updateUser = updateUser
) {
  const updated: User[] = [];
  const failed: { user: User; message: string }[] = [];
  for (const user of members) {
    try {
      updated.push(await update(user, patch));
    } catch (cause) {
      failed.push({ user, message: errorMessage(cause) });
    }
  }
  return { updated, failed };
}
