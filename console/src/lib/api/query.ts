export function compactQuery<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(
      ([, item]) => item !== '' && item !== undefined && item !== null
    )
  ) as T;
}
