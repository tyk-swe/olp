export const PASSWORD_MIN_LENGTH = 12;
export const PASSWORD_MAX_LENGTH = 1024;

export const PASSWORD_TOO_SHORT = `Use at least ${PASSWORD_MIN_LENGTH} characters.`;
export const PASSWORD_TOO_LONG = `Use ${PASSWORD_MAX_LENGTH.toLocaleString('en-US')} characters or fewer.`;

export type PasswordPolicyErrors = {
  password?: string;
  confirmPassword?: string;
};

/**
 * A password that already fails on length is not also reported as mismatched,
 * so the operator sees one actionable message at a time.
 */
export function validatePasswordPolicy(
  password: string,
  confirmation: string,
  mismatch = 'Passwords do not match.'
): PasswordPolicyErrors {
  if (password.length < PASSWORD_MIN_LENGTH)
    return { password: PASSWORD_TOO_SHORT };
  if (password.length > PASSWORD_MAX_LENGTH)
    return { password: PASSWORD_TOO_LONG };
  return password === confirmation ? {} : { confirmPassword: mismatch };
}
