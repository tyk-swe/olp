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
  else if (Array.from(displayName).length > 100) errors.displayName = 'Use 100 characters or fewer.';
  const passwordLength = Array.from(values.password).length;
  const passwordValid = passwordLength >= 12 && passwordLength <= 1024;
  if (passwordLength < 12) errors.password = 'Use at least 12 characters.';
  else if (passwordLength > 1024) errors.password = 'Use 1,024 characters or fewer.';
  if (passwordValid && values.password !== values.confirmPassword) {
    errors.confirmPassword = 'Passwords do not match.';
  }
  return errors;
}
