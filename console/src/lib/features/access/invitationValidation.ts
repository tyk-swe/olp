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
  else if (displayName.length > 100) errors.displayName = 'Use 100 characters or fewer.';
  const passwordValid = values.password.length >= 12 && values.password.length <= 1024;
  if (values.password.length < 12) errors.password = 'Use at least 12 characters.';
  else if (values.password.length > 1024) errors.password = 'Use 1,024 characters or fewer.';
  if (passwordValid && values.password !== values.confirmPassword) {
    errors.confirmPassword = 'Passwords do not match.';
  }
  return errors;
}
