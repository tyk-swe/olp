import { validateDisplayName } from '#lib/displayNamePolicy.js';
import { validated, type Validation } from '#lib/editing/validation.js';
import { validatePasswordPolicy } from '#lib/passwordPolicy.js';

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

export type InvitationDraft = {
  email: string;
  role: string;
  expiresInHours: number;
};
export type InvitationField = keyof InvitationDraft;

export const invitationServerFields: Readonly<Record<string, InvitationField>> =
  { email: 'email', role: 'role', expires_in_hours: 'expiresInHours' };

export function validateInvitation(
  draft: InvitationDraft
): Validation<InvitationDraft, InvitationField> {
  const email = draft.email.trim();
  return validated<InvitationDraft, InvitationField>(
    {
      email: email.includes('@') ? undefined : 'Enter a valid email address.'
    },
    () => ({ ...draft, email })
  );
}
