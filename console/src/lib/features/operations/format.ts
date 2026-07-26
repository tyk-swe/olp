// `Intl` formatter construction dominates the cost of formatting a large table
// (the usage chart's data table can render thousands of buckets, each calling
// two formatters). The options never vary per call, so build each one once and
// memoise the currency variants.
const dateTimeFormat = new Intl.DateTimeFormat(undefined, {
  dateStyle: 'medium',
  timeStyle: 'medium'
});

const compactFormat = new Intl.NumberFormat(undefined, {
  notation: 'compact',
  maximumFractionDigits: 1
});

const currencyFormats = new Map<string, Intl.NumberFormat>();

function currencyFormat(currency: string, fractional: boolean): Intl.NumberFormat {
  const key = `${currency}:${fractional}`;
  let format = currencyFormats.get(key);
  if (!format) {
    format = new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency,
      minimumFractionDigits: fractional ? 4 : 2,
      maximumFractionDigits: fractional ? 6 : 2
    });
    currencyFormats.set(key, format);
  }
  return format;
}

export function formatDate(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return dateTimeFormat.format(date);
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

export function formatCost(value?: string | null, currency = 'USD'): string {
  if (value === null || value === undefined || value === '') return 'Unpriced';
  const number = Number(value);
  if (!Number.isFinite(number)) return `${value} ${currency}`;
  return currencyFormat(currency, number < 0.01).format(number);
}

export function statusTone(status?: number | null, errorClass?: string | null) {
  if (errorClass || (status !== null && status !== undefined && status >= 500)) return 'danger';
  if (status === 429 || (status !== null && status !== undefined && status >= 400)) return 'warning';
  if (status !== null && status !== undefined && status >= 200 && status < 400) return 'success';
  return '';
}

export function statusLabel(status?: number | null, errorClass?: string | null): string {
  if (errorClass) return errorClass;
  if (status === null || status === undefined) return 'In progress';
  return String(status);
}
