export function optionalDecimal(value: string): string | null {
  const decimal = value.trim();
  if (!decimal) return null;
  if (!/^\d+(?:\.\d+)?$/.test(decimal)) {
    throw new Error('Enter a non-negative decimal number.');
  }
  return decimal;
}

import type { components } from '$lib/api/schema';

export function decimalText(value: string): string | null {
  return optionalDecimal(value);
}

const toolNamePattern = /^[a-z][a-z0-9_]{0,63}$/;

export function toolNameText(value: string): string {
  const name = value.trim();
  if (!toolNamePattern.test(name)) {
    throw new Error(
      'Tool names are lowercase letters, digits and underscores, starting with a letter.'
    );
  }
  return name;
}

/** Tool fee rows become the tool_calls map; rows with no name are dropped so an
 * untouched spare row does not block submission. */
export function toolFeeRows(
  rows: { name: string; fee: string }[]
): Record<string, string> | null {
  const calls: Record<string, string> = {};
  for (const row of rows) {
    if (!row.name.trim()) {
      if (row.fee.trim())
        throw new Error('Enter a tool name for every tool fee.');
      continue;
    }
    const name = toolNameText(row.name);
    if (name in calls) throw new Error(`Tool ${name} is listed twice.`);
    const fee = optionalDecimal(row.fee);
    if (!fee) throw new Error(`Enter a fee for tool ${name}.`);
    calls[name] = fee;
  }
  if (Object.keys(calls).length === 0) return null;
  if (Object.keys(calls).length > 16)
    throw new Error('At most 16 tool fees per price.');
  return calls;
}

/** A tier names the same per-million rate fields as its entry: every field the
 * entry fills is required on each tier row. */
export function tierRows(
  rows: { threshold: string; rates: Record<string, string> }[],
  entryFields: readonly string[]
): components['schemas']['PriceTier'][] | null {
  if (rows.length === 0) return null;
  if (rows.length > 8) throw new Error('At most 8 tiers per price.');
  if (entryFields.length === 0)
    throw new Error('Tiers replace the per-million rates; enter a rate first.');
  let previous = 0n;
  return rows.map((row, index) => {
    const threshold = row.threshold.trim();
    if (!/^\d+$/.test(threshold) || BigInt(threshold) <= 0n)
      throw new Error(`Tier ${index + 1} needs a positive token threshold.`);
    const above = BigInt(threshold);
    if (index > 0 && above <= previous)
      throw new Error('Tier thresholds must be strictly increasing.');
    previous = above;
    const tier: components['schemas']['PriceTier'] = {
      above_input_tokens: Number(above)
    };
    for (const field of entryFields) {
      const value = optionalDecimal(row.rates[field] ?? '');
      if (!value)
        throw new Error(
          `Tier ${index + 1} needs a ${field.replaceAll('_', ' ')} rate.`
        );
      (tier as Record<string, unknown>)[field] = value;
    }
    return tier;
  });
}

const dayTime = /^([01]\d|2[0-3]):[0-5]\d$/;

function windowTime(value: string, label: string): string {
  const time = value.trim();
  if (!dayTime.test(time) && time !== '24:00')
    throw new Error(`${label} is a HH:MM time.`);
  return time;
}

export function windowsDraft(
  zone: string,
  periods: { days: number[]; start: string; end: string; multiplier: string }[]
): components['schemas']['PriceWindows'] | null {
  const kept = periods.filter(
    (period) =>
      period.days.length > 0 ||
      period.start.trim() ||
      period.end.trim() ||
      period.multiplier.trim()
  );
  if (!zone.trim() && kept.length === 0) return null;
  const timeZone = zone.trim();
  if (!timeZone) throw new Error('Enter an IANA time zone for the windows.');
  try {
    new Intl.DateTimeFormat('en', { timeZone });
  } catch {
    throw new Error(`Unknown IANA time zone ${timeZone}.`);
  }
  if (kept.length === 0) throw new Error('Add at least one window period.');
  if (kept.length > 32) throw new Error('At most 32 window periods.');
  return {
    time_zone: timeZone,
    periods: kept.map((period, index) => {
      if (period.days.length === 0)
        throw new Error(`Window ${index + 1} needs at least one day.`);
      const start = windowTime(period.start, `Window ${index + 1} start`);
      if (start === '24:00')
        throw new Error(`Window ${index + 1} starts before 24:00.`);
      const end = windowTime(period.end, `Window ${index + 1} end`);
      if (end !== '24:00' && end <= start)
        throw new Error(`Window ${index + 1} ends after it starts.`);
      const multiplier = optionalDecimal(period.multiplier);
      if (!multiplier)
        throw new Error(`Window ${index + 1} needs a multiplier.`);
      return {
        days: [...period.days].sort((a, b) => a - b),
        start,
        end,
        multiplier
      };
    })
  };
}
