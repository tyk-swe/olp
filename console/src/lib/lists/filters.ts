import { instant } from '$lib/api/query';
import { dateTimeLocalValue } from '$lib/format';

/** Canonical UUID text, as accepted by the identifier filters. */
export const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Whether a time filter is a real calendar instant. URL values must carry a
 * timezone so a shared link means the same window for every operator; form
 * values may be local `datetime-local` text.
 */
export function timeValid(value: string, fromUrl = false): boolean {
  const match =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2})?$/i.exec(
      value
    );
  if (!match || (fromUrl && !match[8]) || (match[8] && !match[6])) return false;
  if (instant(value) === undefined) return false;
  // Date parsing accepts what it can roll over (2023-02-30, T24:00), so the
  // day-of-month and hour ranges are checked explicitly.
  const [year, month, day, hour] = match.slice(1, 5).map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return day <= days[month - 1] && hour <= 23;
}

/**
 * The query instant for a time field: the exact applied bound when the form
 * still shows it (so re-applying never rounds sub-second precision away),
 * otherwise the field converted to an instant, or nothing while it is invalid.
 */
export function preservedTime(
  value: string,
  applied?: string
): string | undefined {
  value = value.trim();
  if (applied && value === dateTimeLocalValue(applied)) return applied;
  if (!timeValid(value)) return undefined;
  return /(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? value : instant(value);
}

/** A sortable key that keeps nanosecond fractions `Date` would drop. */
export function timeOrder(value: string): string {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? '';
  return `${new Date(value).toISOString().slice(0, 19)}.${fraction.padEnd(9, '0')}`;
}

/** The `datetime-local` text for a URL instant, or the raw text when invalid. */
export function formTimeValue(value: string): string {
  return timeValid(value, true) ? dateTimeLocalValue(value) : value;
}

/**
 * A time field is a `datetime-local` picker while empty or holding local
 * text, and falls back to a text input so malformed or timezone-qualified
 * values stay visible instead of being blanked by the browser.
 */
export function timeInputType(value: string): 'text' | 'datetime-local' {
  return value && (!timeValid(value) || timeValid(value, true))
    ? 'text'
    : 'datetime-local';
}

export function uuidProblem(
  fields: ReadonlyArray<readonly [value: string, label: string]>
): string | null {
  for (const [value, label] of fields) {
    if (value.trim() && !UUID.test(value.trim()))
      return `${label} must be a UUID: ${value}`;
  }
  return null;
}

export function timeProblem(
  fields: ReadonlyArray<readonly [value: string, label: string]>,
  fromUrl: boolean
): string | null {
  for (const [value, label] of fields) {
    if (value.trim() && !timeValid(value, fromUrl))
      return `${label} must be a valid time${fromUrl ? ' with a timezone' : ''}: ${value}`;
  }
  return null;
}

export function rangeProblem(
  after: string | undefined,
  before: string | undefined,
  message: string
): string | null {
  if (!after || !before || timeOrder(after) < timeOrder(before)) return null;
  return message;
}

/** Reads every form field from its query parameter, trimmed. */
export function readForm<Form extends Record<string, string>>(
  search: URLSearchParams,
  params: { readonly [K in keyof Form]: string }
): Form {
  return Object.fromEntries(
    Object.entries(params).map(([field, name]) => [
      field,
      search.get(name)?.trim() ?? ''
    ])
  ) as Form;
}

/** Serializes the non-empty query parameters, in the given order. */
export function querySearch(
  applied: Record<string, string | number | undefined>,
  names: readonly string[]
): string {
  const search = new URLSearchParams();
  for (const name of names) {
    const value = applied[name];
    if (value !== undefined && value !== '') search.set(name, String(value));
  }
  return search.toString();
}
