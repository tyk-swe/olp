export type ApiKeyFormValue = {
  name: string;
  requestsPerMinute?: number;
  tokensPerMinute?: number;
  maxConcurrency?: number;
  dailyCostLimit?: string;
  monthlyCostLimit?: string;
  /** `datetime-local` control value, or an empty string when no expiry is set. */
  expiresAt?: string;
  /** Injected by tests; defaults to the current instant. */
  now?: Date;
};

export function validateApiKey(value: ApiKeyFormValue): Record<string, string> {
  const errors: Record<string, string> = {};
  const name = value.name.trim();
  if (!name) errors.name = 'Enter a name.';
  else if (name.length > 100) errors.name = 'Use 100 characters or fewer.';

  for (const field of [
    'requestsPerMinute',
    'tokensPerMinute',
    'maxConcurrency'
  ] as const) {
    const limit = value[field];
    if (limit !== undefined && (!Number.isInteger(limit) || limit < 1)) {
      errors[field] = 'Use a positive whole number.';
    }
  }

  for (const field of ['dailyCostLimit', 'monthlyCostLimit'] as const) {
    const limit = value[field]?.trim();
    if (
      limit &&
      (!/^\d{1,12}(?:\.\d{1,12})?$/.test(limit) || !/[1-9]/.test(limit))
    ) {
      errors[field] =
        'Use a positive decimal with at most 12 digits before and after the decimal point.';
    }
  }

  if (value.expiresAt) {
    const expiry = new Date(value.expiresAt);
    if (Number.isNaN(expiry.valueOf())) {
      errors.expiresAt = 'Enter a valid expiry date and time.';
    } else if (expiry.valueOf() <= (value.now ?? new Date()).valueOf()) {
      // A key created already expired authenticates nothing, so it is a typo
      // rather than an intent worth persisting.
      errors.expiresAt = 'Choose an expiry in the future.';
    }
  }
  return errors;
}
