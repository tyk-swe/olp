import { validatePasswordPolicy } from '$lib/passwordPolicy';

export type OwnerFormValues = {
  displayName: string;
  installationName: string;
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
  const installationName = values.installationName.trim();
  if (!displayName) errors.displayName = 'Enter a display name.';
  else if (displayName.length > 100)
    errors.displayName = 'Use 100 characters or fewer.';
  // An empty installation name is omitted from the request so the API applies
  // its own default; only a name the operator actually typed is length-checked.
  if (installationName.length > 100) {
    errors.installationName = 'Use 100 characters or fewer.';
  }
  if (!email) errors.email = 'Enter your email address.';
  // Client-side shape check only; the API remains the authoritative email validator.
  else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
    errors.email = 'Enter a valid email address.';
  } else if (email.length > 254) errors.email = 'Use 254 characters or fewer.';
  Object.assign(
    errors,
    validatePasswordPolicy(values.password, values.confirmPassword)
  );
  if (!values.setupToken) errors.setupToken = 'Enter the setup token.';
  return errors;
}
