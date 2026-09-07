import { validateDisplayName as displayNameProblem } from '$lib/displayNamePolicy';
import { validatePasswordPolicy } from '$lib/passwordPolicy';

/**
 * The profile editor surfaces a rule failure by throwing; the rule itself is
 * shared with owner setup and invitation acceptance.
 */
export function validateDisplayName(value: string) {
  const problem = displayNameProblem(value);
  if (problem) throw new Error(problem);
  return value.trim();
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
