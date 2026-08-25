export function formatDate(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'medium'
  }).format(date);
}

export function formatDay(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  // Daily buckets are UTC midnights. Naming them in the viewer's zone would
  // label every bucket with the previous calendar day west of UTC.
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeZone: 'UTC'
  }).format(date);
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
  return new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(
    number
  );
}

export function formatInteger(value: number | string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const number = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(number)) return String(value);
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(number);
}

/**
 * A missing currency means the record was never priced in a known unit, so the
 * amount is grouped without a symbol rather than being labelled as dollars.
 */
export function formatCost(value?: string | null, currency?: string | null): string {
  if (value === null || value === undefined || value === '') return 'Unpriced';
  const number = Number(value);
  if (!Number.isFinite(number)) return currency ? `${value} ${currency}` : String(value);
  const fractionDigits = {
    minimumFractionDigits: number < 0.01 ? 4 : 2,
    maximumFractionDigits: number < 0.01 ? 6 : 2
  };
  if (!currency) return new Intl.NumberFormat(undefined, fractionDigits).format(number);
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency,
    ...fractionDigits
  }).format(number);
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
