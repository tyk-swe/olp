import * as v from 'valibot';

export const ownerFormSchema = v.pipe(
  v.object({
    displayName: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, 'Enter your name.'),
      v.maxLength(100, 'Use 100 characters or fewer.')
    ),
    email: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, 'Enter your email address.'),
      v.email('Enter a valid email address.'),
      v.maxLength(254, 'Use 254 characters or fewer.')
    ),
    password: v.pipe(
      v.string(),
      v.minLength(12, 'Use at least 12 characters.'),
      v.maxLength(1024, 'Use 1,024 characters or fewer.')
    ),
    confirmPassword: v.string(),
    setupToken: v.pipe(v.string(), v.minLength(1, 'Enter the setup token.'))
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

export type OwnerFormValues = v.InferInput<typeof ownerFormSchema>;
export type OwnerFormErrors = Partial<Record<keyof OwnerFormValues, string>>;

export function validateOwner(values: OwnerFormValues): OwnerFormErrors {
  const result = v.safeParse(ownerFormSchema, values, { abortPipeEarly: true });
  if (result.success) return {};

  const errors: OwnerFormErrors = {};
  for (const issue of result.issues) {
    const path = v.getDotPath(issue) as keyof OwnerFormValues | null;
    if (path && !errors[path]) errors[path] = issue.message;
  }
  return errors;
}
