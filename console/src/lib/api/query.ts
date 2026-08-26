/**
 * The console's date filters are local-time `datetime-local` values; the API
 * compares instants. A half-typed date stays out of the query rather than
 * being sent as an invalid bound the backend would reject.
 */
export function instant(value: string): string | undefined {
  if (!value.trim()) return undefined;
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? undefined : date.toISOString();
}

export function compactQuery<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(
      ([, item]) => item !== '' && item !== undefined && item !== null
    )
  ) as T;
}
