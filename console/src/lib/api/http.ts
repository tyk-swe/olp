type FieldErrors = Record<string, string[]>;

export type ProblemDetails = {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  errors?: FieldErrors;
};

export class ApiProblem extends Error {
  readonly problem: ProblemDetails;

  constructor(problem: ProblemDetails) {
    super(problem.detail ?? problem.title);
    this.name = 'ApiProblem';
    this.problem = problem;
  }
}

export const ETAG_MISMATCH_TYPE = 'https://openllmproxy.dev/problems/etag_mismatch';

export function isEtagMismatch(error: unknown): error is ApiProblem {
  return (
    error instanceof ApiProblem &&
    error.problem.status === 412 &&
    error.problem.type === ETAG_MISMATCH_TYPE
  );
}

function optionalString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function fieldErrors(value: unknown): FieldErrors | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined;
  const entries = Object.entries(value);
  if (
    !entries.every(
      ([, messages]) =>
        Array.isArray(messages) && messages.every((message) => typeof message === 'string')
    )
  ) {
    return undefined;
  }
  return Object.fromEntries(entries) as FieldErrors;
}

function apiProblem(error: unknown, response: Response): ApiProblem {
  const value = error && typeof error === 'object' ? (error as Record<string, unknown>) : {};
  const status =
    typeof value.status === 'number' && Number.isInteger(value.status)
      ? value.status
      : response.status;
  return new ApiProblem({
    type: optionalString(value.type) ?? 'about:blank',
    title: optionalString(value.title) ?? `Request failed (${response.status})`,
    status,
    detail: optionalString(value.detail),
    instance: optionalString(value.instance),
    errors: fieldErrors(value.errors)
  });
}

export function throwApiProblem(error: unknown, response: Response): never {
  throw apiProblem(error, response);
}

export function ensureSuccess(error: unknown, response: Response): void {
  if (!response.ok) throwApiProblem(error, response);
}

export function result<T>(
  data: T | null | undefined,
  error: unknown,
  response: Response
): NonNullable<T> {
  if (!response.ok) throwApiProblem(error, response);
  if (data !== undefined && data !== null) return data;
  throw new ApiProblem({
    type: 'urn:olp:problem:invalid-api-response',
    title: 'The API response did not include the expected JSON body',
    status: 502
  });
}

const BARE_UUID_ETAG =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Serialize a bare API UUID as one strong HTTP entity tag for If-Match. */
export function serializeIfMatch(value: string): string {
  return BARE_UUID_ETAG.test(value) ? `"${value}"` : value;
}

/**
 * First message per field from a validation problem's `errors` map.
 *
 * The server's `Problem::validation` always sets the same generic title and
 * detail ("One or more fields are invalid."), so `errors` holds the only
 * actionable text. Returns an empty object for non-validation failures.
 */
export function problemFieldErrors(error: unknown): Record<string, string> {
  if (!(error instanceof ApiProblem) || !error.problem.errors) return {};
  const flattened: Record<string, string> = {};
  for (const [field, messages] of Object.entries(error.problem.errors)) {
    if (messages[0]) flattened[field] = messages[0];
  }
  return flattened;
}

/**
 * Human-readable text for a failed request, including any field-level reasons.
 *
 * Without the `errors` map an operator only ever sees "One or more fields are
 * invalid." with no indication of which field or why.
 */
export function problemMessage(error: unknown, fallback: string): string {
  if (!(error instanceof ApiProblem)) {
    return error instanceof Error ? error.message : fallback;
  }
  const base = error.problem.detail ?? error.problem.title;
  const fields = Object.entries(problemFieldErrors(error));
  if (!fields.length) return base;
  return `${base} ${fields.map(([field, message]) => `${field}: ${message}`).join(' ')}`;
}
