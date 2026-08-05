export function validateDisplayName(value: string) {
  const displayName = value.trim();
  if (!displayName) throw new Error('Enter your display name.');
  if (displayName.length > 100) throw new Error('Use 100 characters or fewer.');
  return displayName;
}

export function validatePassword(
  current: string,
  next: string,
  confirmation: string
) {
  if (!current) throw new Error('Enter your current password.');
  const password = validateNewPassword(next, confirmation);
  if (current === password)
    throw new Error('Choose a password different from the current password.');
  return password;
}

export function validateNewPassword(next: string, confirmation: string) {
  if (next.length < 12) throw new Error('Use at least 12 characters.');
  if (next.length > 1024) throw new Error('Use 1,024 characters or fewer.');
  if (next !== confirmation) throw new Error('New passwords do not match.');
  return next;
}
