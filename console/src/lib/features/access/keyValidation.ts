type ApiKeyFormValue = {
  name: string;
  requestsPerMinute?: number;
  tokensPerMinute?: number;
  maxConcurrency?: number;
};

export function validateApiKey(value: ApiKeyFormValue): Record<string, string> {
  const errors: Record<string, string> = {};
  const name = value.name.trim();
  if (!name) errors.name = 'Enter a name.';
  else if (Array.from(name).length > 100) errors.name = 'Use 100 characters or fewer.';

  for (const field of ['requestsPerMinute', 'tokensPerMinute', 'maxConcurrency'] as const) {
    const limit = value[field];
    if (limit !== undefined && (!Number.isInteger(limit) || limit < 1)) {
      errors[field] = 'Use a positive whole number.';
    }
  }
  return errors;
}
