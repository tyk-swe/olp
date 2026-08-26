/**
 * The console ships a single English interface, so figures are formatted in one
 * fixed locale instead of the host's: a viewer in de-DE would otherwise read
 * German month names beside the hard-coded English unit suffixes below, and the
 * same byte count would group differently from one operator's machine to the
 * next. Time zone still follows the viewer, which is what a timestamp needs.
 */
const LOCALE = 'en-US';

// Constructing an Intl formatter is the expensive part, and every option here
// is fixed, so each one is built once for the module rather than per table row.
const dateTimeFormat = new Intl.DateTimeFormat(LOCALE, {
  dateStyle: 'medium',
  timeStyle: 'medium'
});
// Daily buckets are UTC midnights. Naming them in the viewer's zone would
// label every bucket with the previous calendar day west of UTC.
const dayFormat = new Intl.DateTimeFormat(LOCALE, { dateStyle: 'medium', timeZone: 'UTC' });
const compactFormat = new Intl.NumberFormat(LOCALE, {
  notation: 'compact',
  maximumFractionDigits: 1
});
const integerFormat = new Intl.NumberFormat(LOCALE, { maximumFractionDigits: 0 });
const fixedFormats = [0, 1, 2].map(
  (digits) =>
    new Intl.NumberFormat(LOCALE, {
      minimumFractionDigits: digits,
      maximumFractionDigits: digits
    })
);

export function formatDate(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return dateTimeFormat.format(date);
}

export function formatDay(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return dayFormat.format(date);
}

export function dateTimeLocalValue(value: Date | string): string {
  const date = typeof value === 'string' ? new Date(value) : value;
  if (Number.isNaN(date.valueOf())) return '';
  const local = new Date(date.valueOf() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

export function formatCompact(value: number | string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const number = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(number)) return String(value);
  return compactFormat.format(number);
}

export function formatInteger(value: number | string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const number = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(number)) return String(value);
  return integerFormat.format(number);
}

// Currency and precision vary per row, so cost formatters are memoized instead
// of hoisted; a usage table repeats a handful of combinations across hundreds
// of rows.
const costFormats = new Map<string, Intl.NumberFormat>();

function costFormat(currency: string | null | undefined, subCent: boolean): Intl.NumberFormat {
  const key = `${currency ?? ''}|${subCent}`;
  const existing = costFormats.get(key);
  if (existing) return existing;
  const fractionDigits = {
    minimumFractionDigits: subCent ? 4 : 2,
    maximumFractionDigits: subCent ? 6 : 2
  };
  const format = new Intl.NumberFormat(
    LOCALE,
    currency ? { style: 'currency', currency, ...fractionDigits } : fractionDigits
  );
  costFormats.set(key, format);
  return format;
}

/**
 * A missing currency means the record was never priced in a known unit, so the
 * amount is grouped without a symbol rather than being labelled as dollars.
 */
export function formatCost(value?: string | null, currency?: string | null): string {
  if (value === null || value === undefined || value === '') return 'Unpriced';
  const number = Number(value);
  if (!Number.isFinite(number)) return currency ? `${value} ${currency}` : String(value);
  return costFormat(currency, number < 0.01).format(number);
}

export function statusTone(status?: number | null, errorClass?: string | null) {
  if (errorClass || (status !== null && status !== undefined && status >= 500)) return 'danger';
  if (status !== null && status !== undefined && status >= 400) return 'warning';
  if (status !== null && status !== undefined && status >= 200 && status < 400) return 'success';
  return '';
}

export function statusLabel(status?: number | null, errorClass?: string | null): string {
  if (errorClass) return errorClass;
  if (status === null || status === undefined) return 'In progress';
  return String(status);
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/**
 * Binary units, because every byte figure the console shows comes from a
 * capacity or a spool the operator configured in KiB/MiB/GiB.
 */
export function formatBytes(value?: number | string | null): string {
  if (value === null || value === undefined || value === '') return '—';
  const number = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(number)) return '—';
  const sign = number < 0 ? '-' : '';
  let size = Math.abs(number);
  let unit = 0;
  while (size >= 1024 && unit < BYTE_UNITS.length - 1) {
    size /= 1024;
    unit += 1;
  }
  const digits = unit === 0 || size >= 100 ? 0 : size >= 10 ? 1 : 2;
  return `${sign}${fixedFormats[digits].format(size)} ${BYTE_UNITS[unit]}`;
}
