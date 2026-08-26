import { validatePasswordPolicy } from '$lib/passwordPolicy';

export type InvitationAcceptanceValues = {
  displayName: string;
  password: string;
  confirmPassword: string;
};
export type InvitationAcceptanceErrors = Partial<
  Record<keyof InvitationAcceptanceValues, string>
>;

export function validateInvitationAcceptance(
  values: InvitationAcceptanceValues
): InvitationAcceptanceErrors {
  const errors: InvitationAcceptanceErrors = {};
  const displayName = values.displayName.trim();
  if (!displayName) errors.displayName = 'Enter your display name.';
  else if (displayName.length > 100)
    errors.displayName = 'Use 100 characters or fewer.';
  Object.assign(
    errors,
    validatePasswordPolicy(values.password, values.confirmPassword)
  );
  return errors;
}
