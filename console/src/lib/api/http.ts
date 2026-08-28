type FieldErrors = Record<string, string[]>;

export type ProblemDetails = {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  errors?: FieldErrors;
  /** Machine-readable classification per field message, parallel to `errors`. */
  errorCodes?: FieldErrors;
};

/** One rejected field message with the code the API classified it under. */
export type FieldIssue = { field: string; message: string; code?: string };

export class ApiProblem extends Error {
  readonly problem: ProblemDetails;

  constructor(problem: ProblemDetails) {
    super(problem.detail ?? problem.title);
    this.name = 'ApiProblem';
    this.problem = problem;
  }
}

const ETAG_MISMATCH_TYPE = 'https://openllmproxy.dev/problems/etag_mismatch';

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
  if (!value || typeof value !== 'object' || Array.isArray(value))
    return undefined;
  const entries = Object.entries(value);
  if (
    !entries.every(
      ([, messages]) =>
        Array.isArray(messages) &&
        messages.every((message) => typeof message === 'string')
    )
  ) {
    return undefined;
  }
  return Object.fromEntries(entries) as FieldErrors;
}

function apiProblem(error: unknown, response: Response): ApiProblem {
  const value =
    error && typeof error === 'object'
      ? (error as Record<string, unknown>)
      : {};
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
    errors: fieldErrors(value.errors),
    errorCodes: fieldErrors(value.error_codes)
  });
}

export type CursorPage<T> = { items: T[]; nextCursor: string | null };

/**
 * Unwraps a management list envelope into the console's cursor page. The
 * `data` key is accepted until every list endpoint emits `items`.
 */
export function pageResult<T>(page: {
  items?: T[];
  data?: T[];
  next_cursor?: string | null;
}): CursorPage<T> {
  return {
    items: page.items ?? page.data ?? [],
    nextCursor: page.next_cursor ?? null
  };
}

export function ensureSuccess(error: unknown, response: Response): void {
  if (!response.ok) throw apiProblem(error, response);
}

export function result<T>(
  data: T | null | undefined,
  error: unknown,
  response: Response
): NonNullable<T> {
  if (!response.ok) throw apiProblem(error, response);
  if (data !== undefined && data !== null) return data;
  throw new ApiProblem({
    type: 'urn:olp:problem:invalid-api-response',
    title: 'The API response did not include the expected JSON body',
    status: 502
  });
}

/**
 * Flattens a validation Problem into one entry per rejected field message.
 * Codes line up with messages by position, and a message without one keeps
 * `code` undefined rather than borrowing its neighbour's. The API pads
 * `error_codes` with an empty string wherever a message is uncoded, so that
 * placeholder is read as "no code" rather than as a classification named "".
 */
export function fieldIssues(error: unknown): FieldIssue[] {
  if (!(error instanceof ApiProblem)) return [];
  const { errors, errorCodes } = error.problem;
  if (!errors) return [];
  return Object.entries(errors).flatMap(([field, messages]) =>
    messages.map((message, index) => ({
      field,
      message,
      code: errorCodes?.[field]?.[index] || undefined
    }))
  );
}

/**
 * Maps a validation Problem's snake_case field names onto a form's own keys,
 * keeping the first message per field. A field the form does not render is
 * dropped, so the caller can fall back to the problem's summary when nothing
 * mapped.
 */
export function applyServerFieldErrors<Key extends string>(
  error: unknown,
  fields: Readonly<Record<string, Key>>
): Partial<Record<Key, string>> {
  const mapped: Partial<Record<Key, string>> = {};
  if (!(error instanceof ApiProblem)) return mapped;
  for (const [field, messages] of Object.entries(error.problem.errors ?? {})) {
    const local = fields[field];
    if (local && messages[0]) mapped[local] = messages[0];
  }
  return mapped;
}

export function errorMessage(
  error: unknown,
  fallback = 'The control API could not complete the request.'
): string {
  return error instanceof Error ? error.message : fallback;
}

const BARE_UUID_ETAG =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Serialize a bare API UUID as one strong HTTP entity tag for If-Match. */
export function serializeIfMatch(value: string): string {
  return BARE_UUID_ETAG.test(value) ? `"${value}"` : value;
}
