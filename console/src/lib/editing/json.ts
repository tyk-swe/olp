/** Narrows a parsed JSON value to a plain object: not null and not an array. */
export function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Parses JSON text into a plain object, or null when the text is not valid
 * JSON or does not hold an object. Callers choose their own fallback.
 */
export function parseJsonObject(text: string): Record<string, unknown> | null {
  try {
    const parsed: unknown = JSON.parse(text);
    return isJsonObject(parsed) ? parsed : null;
  } catch {
    return null;
  }
}
