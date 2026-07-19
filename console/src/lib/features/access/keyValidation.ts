import * as v from 'valibot';

const optionalLimit = v.optional(v.pipe(v.number(), v.integer(), v.minValue(1)));

export const apiKeySchema = v.object({
  name: v.pipe(v.string(), v.trim(), v.minLength(1, 'Enter a name.'), v.maxLength(100, 'Use 100 characters or fewer.')),
  requestsPerMinute: optionalLimit,
  tokensPerMinute: optionalLimit,
  maxConcurrency: optionalLimit
});

export type ApiKeyFormValue = v.InferInput<typeof apiKeySchema>;

export function validateApiKey(value: ApiKeyFormValue): Record<string, string> {
  const result = v.safeParse(apiKeySchema, value);
  if (result.success) return {};
  return Object.fromEntries(result.issues.map((issue) => [issue.path?.[0]?.key ?? 'form', issue.message]));
}
