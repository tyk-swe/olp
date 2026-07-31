export function validateDisplayName(value: string) {
  const displayName = value.trim();
  if (!displayName) throw new Error('Enter your display name.');
  if (Array.from(displayName).length > 100) throw new Error('Use 100 characters or fewer.');
  return displayName;
}

export function validatePassword(current: string, next: string, confirmation: string) {
  if (!current) throw new Error('Enter your current password.');
  const password = validateNewPassword(next, confirmation);
  if (current === password) throw new Error('Choose a password different from the current password.');
  return password;
}

export function validateNewPassword(next: string, confirmation: string) {
  const length = Array.from(next).length;
  if (length < 12) throw new Error('Use at least 12 characters.');
  if (length > 1024) throw new Error('Use 1,024 characters or fewer.');
  if (next !== confirmation) throw new Error('New passwords do not match.');
  return next;
}

export function optionalDecimal(value: string): string | null {
  const decimal = value.trim();
  if (!decimal) return null;
  if (!/^\d+(?:\.\d+)?$/.test(decimal)) {
    throw new Error('Enter a non-negative decimal number.');
  }
  return decimal;
}
