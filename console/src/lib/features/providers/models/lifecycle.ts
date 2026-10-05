import type { components } from '$lib/api/schema';

export type ModelLifecycle = components['schemas']['ModelLifecycle'];

/** Retirements this close count as imminent. */
export const RETIREMENT_WARNING_DAYS = 90;

export type LifecycleNotice = {
  tone: 'warning' | 'danger';
  text: string;
};

const DAY = 24 * 60 * 60 * 1000;

function day(date: string): number {
  return Date.parse(`${date}T00:00:00Z`);
}

function startOfDay(today: Date): number {
  return Date.UTC(
    today.getUTCFullYear(),
    today.getUTCMonth(),
    today.getUTCDate()
  );
}

/**
 * What a route editor or model list says about a model the vendor has
 * deprecated or is retiring, as the reference catalog documents it: danger
 * once retired, a warning when the retirement is near or the model is
 * deprecated, and nothing for a distant retirement.
 */
export function lifecycleNotice(
  lifecycle: ModelLifecycle | null | undefined,
  today: Date = new Date()
): LifecycleNotice | null {
  if (!lifecycle) return null;
  const now = startOfDay(today);
  const replacement = lifecycle.replacement
    ? ` Its vendor names ${lifecycle.replacement} as the replacement.`
    : '';
  if (lifecycle.retires_at) {
    const retires = day(lifecycle.retires_at);
    if (retires <= now)
      return {
        tone: 'danger',
        text: `Retired by its vendor on ${lifecycle.retires_at}.${replacement}`
      };
    const days = Math.ceil((retires - now) / DAY);
    if (days <= RETIREMENT_WARNING_DAYS)
      return {
        tone: 'warning',
        text: `Retires on ${lifecycle.retires_at}, in ${days} ${days === 1 ? 'day' : 'days'}.${replacement}`
      };
  }
  if (lifecycle.deprecated_at && day(lifecycle.deprecated_at) <= now)
    return {
      tone: 'warning',
      text: `Deprecated by its vendor on ${lifecycle.deprecated_at}${lifecycle.retires_at ? `; retires on ${lifecycle.retires_at}` : ''}.${replacement}`
    };
  return null;
}
