/**
 * Readiness checkpoint ages, measured against the runbook thresholds in
 * `docs/operations.md`: metadata, outbox, and gateway-epoch checkpoints are
 * stale after 20 seconds, maintenance after 180 seconds. The backend reports
 * both timestamps and pre-computed ages; a timestamp is turned into an age here
 * so an operator reading the page never has to subtract clocks by hand.
 */
export const CHECKPOINT_STALE_SECONDS = 20;
export const MAINTENANCE_STALE_SECONDS = 180;
/**
 * Pending metadata is reclaimable after 30 seconds and scanned every five, so
 * the runbook asks for an investigation once recovery has not begun by 35.
 */
export const PENDING_RECOVERY_SECONDS = 35;

export type AgeStatus = {
  seconds: number | null;
  label: string;
  stale: boolean;
  tone: '' | 'warning';
};

export function secondsSince(
  at: string | null | undefined,
  now: number
): number | null {
  if (!at) return null;
  const parsed = Date.parse(at);
  if (Number.isNaN(parsed)) return null;
  // A checkpoint written by a replica whose clock runs ahead is not negative
  // progress; report it as current rather than as a nonsense age.
  return Math.max(0, Math.round((now - parsed) / 1000));
}

export function formatAge(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return 'Not reported';
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m ago`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h ago`;
}

function status(seconds: number | null, threshold: number): AgeStatus {
  const stale = seconds !== null && seconds > threshold;
  return {
    seconds,
    label: formatAge(seconds),
    stale,
    tone: stale ? 'warning' : ''
  };
}

/** Age of a reported timestamp relative to `now`. */
export function ageStatus(
  at: string | null | undefined,
  now: number,
  threshold: number = CHECKPOINT_STALE_SECONDS
): AgeStatus {
  return status(secondsSince(at, now), threshold);
}

/** Age the backend already computed, so no clock comparison is repeated. */
export function reportedAgeStatus(
  seconds: number | null | undefined,
  threshold: number = CHECKPOINT_STALE_SECONDS
): AgeStatus {
  return status(seconds ?? null, threshold);
}

/**
 * Prefers the age the backend reported and falls back to the timestamp, so a
 * summary that carries only one of the pair is still shown as an age.
 */
export function oldestPendingStatus(
  at: string | null | undefined,
  reportedSeconds: number | null | undefined,
  now: number,
  threshold: number = PENDING_RECOVERY_SECONDS
): AgeStatus {
  return reportedSeconds === null || reportedSeconds === undefined
    ? ageStatus(at, now, threshold)
    : reportedAgeStatus(reportedSeconds, threshold);
}
