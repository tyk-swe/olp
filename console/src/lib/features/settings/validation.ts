export function optionalDecimal(value: string): string | null {
  const decimal = value.trim();
  if (!decimal) return null;
  if (!/^\d+(?:\.\d+)?$/.test(decimal)) {
    throw new Error('Enter a non-negative decimal number.');
  }
  return decimal;
}

export const LIMITS_OUTAGE_POLICIES = ['fail_closed', 'fail_open'] as const;
export type LimitsOutagePolicy = (typeof LIMITS_OUTAGE_POLICIES)[number];

export function isLimitsOutagePolicy(
  value: string
): value is LimitsOutagePolicy {
  return (LIMITS_OUTAGE_POLICIES as readonly string[]).includes(value);
}

/** Retention keys the control API validates as whole days in 1..=3650. */
export const RETENTION_KEYS = [
  'retention.requests_days',
  'retention.usage_days',
  'retention.audit_days'
] as const;
export const RETENTION_MIN_DAYS = 1;
export const RETENTION_MAX_DAYS = 3650;

export function isRetentionKey(key: string): boolean {
  return (RETENTION_KEYS as readonly string[]).includes(key);
}
