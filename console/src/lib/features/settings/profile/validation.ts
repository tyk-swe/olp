import { validatePasswordPolicy } from '$lib/passwordPolicy';

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
  const { password, confirmPassword } = validatePasswordPolicy(
    next,
    confirmation,
    'New passwords do not match.'
  );
  const problem = password ?? confirmPassword;
  if (problem) throw new Error(problem);
  return next;
}
