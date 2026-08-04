export function optionalDecimal(value: string): string | null {
  const decimal = value.trim();
  if (!decimal) return null;
  if (!/^\d+(?:\.\d+)?$/.test(decimal)) {
    throw new Error('Enter a non-negative decimal number.');
  }
  return decimal;
}
