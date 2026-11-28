import { validateDisplayName } from '#lib/displayNamePolicy.js';
import { validated } from '#lib/editing/validation.js';
import { validatePasswordPolicy } from '#lib/passwordPolicy.js';

export type OwnerFormValues = {
  displayName: string;
  installationName: string;
  email: string;
  password: string;
  confirmPassword: string;
  setupToken: string;
};
type OwnerFormErrors = Partial<Record<keyof OwnerFormValues, string>>;
export const ownerServerFields: Readonly<
  Record<string, keyof OwnerFormValues>
> = {
  display_name: 'displayName',
  installation_name: 'installationName',
  email: 'email',
  password: 'password',
  setup_token: 'setupToken'
};

export function validateOwner(values: OwnerFormValues) {
  const errors: OwnerFormErrors = {};
  const email = values.email.trim();
  const installationName = values.installationName.trim();
  const displayNameProblem = validateDisplayName(values.displayName);
  if (displayNameProblem) errors.displayName = displayNameProblem;
  // An empty installation name is omitted from the request so the API applies
  // its own default; only a name the operator actually typed is length-checked.
  if (installationName.length > 100) {
    errors.installationName = 'Use 100 characters or fewer.';
  }
  if (!email) errors.email = 'Enter your email address.';
  // Client-side shape check only; the API remains the authoritative email validator.
  else if (!/^[^\s@]+@[^\s@]+$/.test(email)) {
    errors.email = 'Enter a valid email address.';
  } else if (email.length > 254) errors.email = 'Use 254 characters or fewer.';
  Object.assign(
    errors,
    validatePasswordPolicy(values.password, values.confirmPassword)
  );
  if (!values.setupToken) errors.setupToken = 'Enter the setup token.';
  return validated(errors, () => values);
}
