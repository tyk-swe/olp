import * as v from 'valibot';

export const invitationAcceptanceSchema = v.pipe(
  v.object({
    displayName: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, 'Enter your display name.'),
      v.maxLength(100, 'Use 100 characters or fewer.')
    ),
    password: v.pipe(
      v.string(),
      v.minLength(12, 'Use at least 12 characters.'),
      v.maxLength(1024, 'Use 1,024 characters or fewer.')
    ),
    confirmPassword: v.string()
  }),
  v.forward(
    v.partialCheck(
      [['password'], ['confirmPassword']],
      ({ password, confirmPassword }) => password === confirmPassword,
      'Passwords do not match.'
    ),
    ['confirmPassword']
  )
);

export type InvitationAcceptanceValues = v.InferInput<typeof invitationAcceptanceSchema>;
export type InvitationAcceptanceErrors = Partial<
  Record<keyof InvitationAcceptanceValues, string>
>;

export function validateInvitationAcceptance(
  values: InvitationAcceptanceValues
): InvitationAcceptanceErrors {
  const result = v.safeParse(invitationAcceptanceSchema, values, { abortPipeEarly: true });
  if (result.success) return {};
  const errors: InvitationAcceptanceErrors = {};
  for (const issue of result.issues) {
    const path = v.getDotPath(issue) as keyof InvitationAcceptanceValues | null;
    if (path && !errors[path]) errors[path] = issue.message;
  }
  return errors;
}
