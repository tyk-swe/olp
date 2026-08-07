export function formatDate(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'medium'
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

export function formatCost(value?: string | null, currency = 'USD'): string {
  if (value === null || value === undefined || value === '') return 'Unpriced';
  const number = Number(value);
  if (!Number.isFinite(number)) return `${value} ${currency}`;
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency,
    minimumFractionDigits: number < 0.01 ? 4 : 2,
    maximumFractionDigits: number < 0.01 ? 6 : 2
  }).format(number);
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
