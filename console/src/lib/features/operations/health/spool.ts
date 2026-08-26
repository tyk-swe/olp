import { formatBytes } from '$lib/format';

/**
 * Both spool figures come from the same readiness read, so either one being
 * absent means the volume could not be measured at all — reported as unmeasured
 * rather than as an empty or full disk.
 */
export function spoolUsage(used?: number | null, capacity?: number | null): string {
  if (used === null || used === undefined || capacity === null || capacity === undefined) {
    return '—';
  }
  // A spool with no capacity is a configuration state, not a division to
  // attempt; the share would be Infinity or NaN.
  const share = capacity === 0 ? 'no capacity' : `${((used / capacity) * 100).toFixed(1)}%`;
  return `${formatBytes(used)} of ${formatBytes(capacity)} (${share})`;
}
