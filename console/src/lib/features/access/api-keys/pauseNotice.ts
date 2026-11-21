import type { ApiKeyPauseRollout } from './api';

export function pauseEnforced(
  rollout: ApiKeyPauseRollout | null | undefined
): boolean {
  if (!rollout?.paused || rollout.enforced !== true) return false;
  const sequence = rollout.paused_authority_sequence;
  if (sequence === null || sequence === undefined) return false;
  const replicas = rollout.replicas ?? [];
  if (replicas.length === 0) return false;
  return replicas.every(
    (replica) =>
      replica.authority_sequence !== null &&
      replica.authority_sequence !== undefined &&
      replica.authority_sequence >= sequence
  );
}

export function pauseNotice(
  saved: boolean,
  rollout: ApiKeyPauseRollout | null | undefined
): string | null {
  if (!saved) return null;
  if (pauseEnforced(rollout)) {
    return 'Pause enforced across serving replicas';
  }
  return 'Pause saved. Enforcement across replicas is not yet confirmed.';
}
