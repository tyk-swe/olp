import { validated } from '#lib/editing/validation.js';

export type LoginField = 'email' | 'password';

export function validateLogin(email: string, password: string) {
  const trimmed = email.trim();
  return validated<{ email: string; password: string }, LoginField>(
    {
      ...(trimmed ? {} : { email: 'Enter your email.' }),
      ...(password ? {} : { password: 'Enter your password.' })
    },
    () => ({ email: trimmed, password })
  );
}
