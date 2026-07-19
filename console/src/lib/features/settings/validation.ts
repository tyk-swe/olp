import * as v from 'valibot';

const DisplayNameSchema = v.pipe(
  v.string(),
  v.trim(),
  v.minLength(1, 'Enter your display name.'),
  v.maxLength(100, 'Use 100 characters or fewer.')
);

const PasswordSchema = v.pipe(
  v.string(),
  v.minLength(12, 'Use at least 12 characters.'),
  v.maxLength(1024, 'Use 1,024 characters or fewer.')
);

const DecimalSchema = v.pipe(
  v.string(),
  v.regex(/^\d+(?:\.\d+)?$/, 'Enter a non-negative decimal number.')
);

export function validateDisplayName(value: string) {
  const result = v.safeParse(DisplayNameSchema, value);
  if (!result.success) throw new Error(result.issues[0]?.message ?? 'Invalid display name.');
  return result.output;
}

export function validatePassword(current: string, next: string, confirmation: string) {
  if (!current) throw new Error('Enter your current password.');
  const password = validateNewPassword(next, confirmation);
  if (current === password) throw new Error('Choose a password different from the current password.');
  return password;
}

export function validateNewPassword(next: string, confirmation: string) {
  const result = v.safeParse(PasswordSchema, next);
  if (!result.success) throw new Error(result.issues[0]?.message ?? 'Invalid password.');
  if (next !== confirmation) throw new Error('New passwords do not match.');
  return result.output;
}

export function optionalDecimal(value: string): string | null {
  if (!value.trim()) return null;
  const result = v.safeParse(DecimalSchema, value.trim());
  if (!result.success) throw new Error(result.issues[0]?.message ?? 'Invalid decimal.');
  return result.output;
}
