export type OwnerFormValues = {
  displayName: string;
  email: string;
  password: string;
  confirmPassword: string;
  setupToken: string;
};
export type OwnerFormErrors = Partial<Record<keyof OwnerFormValues, string>>;

export function validateOwner(values: OwnerFormValues): OwnerFormErrors {
  const errors: OwnerFormErrors = {};
  const displayName = values.displayName.trim();
  const email = values.email.trim();
  if (!displayName) errors.displayName = 'Enter your name.';
  else if (displayName.length > 100)
    errors.displayName = 'Use 100 characters or fewer.';
  if (!email) errors.email = 'Enter your email address.';
  // Client-side shape check only; the API remains the authoritative email validator.
  else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
    errors.email = 'Enter a valid email address.';
  } else if (email.length > 254) errors.email = 'Use 254 characters or fewer.';
  const passwordValid =
    values.password.length >= 12 && values.password.length <= 1024;
  if (values.password.length < 12)
    errors.password = 'Use at least 12 characters.';
  else if (values.password.length > 1024)
    errors.password = 'Use 1,024 characters or fewer.';
  if (passwordValid && values.password !== values.confirmPassword) {
    errors.confirmPassword = 'Passwords do not match.';
  }
  if (!values.setupToken) errors.setupToken = 'Enter the setup token.';
  return errors;
}
