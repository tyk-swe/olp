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
