export const DISPLAY_NAME_MAX_LENGTH = 100;

export const DISPLAY_NAME_REQUIRED = 'Enter your display name.';
export const DISPLAY_NAME_TOO_LONG = `Use ${DISPLAY_NAME_MAX_LENGTH} characters or fewer.`;

/**
 * Owner setup, invitation acceptance, and the profile editor all take the same
 * display name under the same API rule, so it is worded once here. Returns the
 * violation, or undefined when the trimmed name is acceptable.
 */
export function validateDisplayName(value: string): string | undefined {
  const displayName = value.trim();
  if (!displayName) return DISPLAY_NAME_REQUIRED;
  if (displayName.length > DISPLAY_NAME_MAX_LENGTH)
    return DISPLAY_NAME_TOO_LONG;
  return undefined;
}
