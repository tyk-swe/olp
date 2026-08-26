import { validateDisplayName } from '$lib/displayNamePolicy';
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
  const displayNameProblem = validateDisplayName(values.displayName);
  if (displayNameProblem) errors.displayName = displayNameProblem;
  Object.assign(
    errors,
    validatePasswordPolicy(values.password, values.confirmPassword)
  );
  return errors;
}
