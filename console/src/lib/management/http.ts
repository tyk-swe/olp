type FieldError = { code: string; message: string };
type FieldErrors = Record<string, FieldError[]>;

export type ProblemDetails = {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  errors?: FieldErrors;
};

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
        messages.every(
          (error) =>
            error &&
            typeof error === 'object' &&
            typeof error.message === 'string' &&
            typeof error.code === 'string'
        )
    )
  ) {
    return undefined;
  }
  return Object.fromEntries(entries) as FieldErrors;
}

export function apiProblem(error: unknown, response: Response): ApiProblem {
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
    errors: fieldErrors(value.errors)
  });
}

export type CursorPage<T> = { items: T[]; nextCursor: string | null };

export type Fetched<T> = { data?: T; error?: unknown; response: Response };

export function pageResult<T>(page: {
  items: T[];
  next_cursor?: string | null;
}): CursorPage<T> {
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export type ApiOutcome<T> =
  | { kind: 'success'; value: T }
  | { kind: 'failure' | 'invalid-response'; problem: ApiProblem }
  | { kind: 'canceled' | 'unavailable'; cause: unknown };

export function responseOutcome<T>(
  fetched: Fetched<T>
): ApiOutcome<NonNullable<T>> {
  if (!fetched.response.ok)
    return {
      kind: 'failure',
      problem: apiProblem(fetched.error, fetched.response)
    };
  if (fetched.data !== undefined && fetched.data !== null)
    return { kind: 'success', value: fetched.data };
  return invalidResponse(
    'The API response did not include the expected JSON body'
  );
}

export function invalidResponse(title: string): ApiOutcome<never> {
  return {
    kind: 'invalid-response',
    problem: new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title,
      status: 502
    })
  };
}

export async function requestOutcome<T>(
  request: () => Promise<Fetched<T>>
): Promise<ApiOutcome<NonNullable<T>>> {
  try {
    return responseOutcome(await request());
  } catch (cause) {
    if (cause instanceof SyntaxError)
      return invalidResponse('The API response is not valid JSON');
    return { kind: abortError(cause) ? 'canceled' : 'unavailable', cause };
  }
}

export function unwrapOutcome<T>(outcome: ApiOutcome<T>): T {
  switch (outcome.kind) {
    case 'success':
      return outcome.value;
    case 'failure':
    case 'invalid-response':
      throw outcome.problem;
    default:
      throw outcome.cause;
  }
}

export function unwrap<T>(fetched: Fetched<T>): NonNullable<T> {
  return unwrapOutcome(responseOutcome(fetched));
}

export function unwrapPage<T>(
  fetched: Fetched<{ items: T[]; next_cursor?: string | null }>
): CursorPage<T> {
  return pageResult(unwrap(fetched));
}

export function ensureOk(fetched: {
  error?: unknown;
  response: Response;
}): void {
  if (!fetched.response.ok) throw apiProblem(fetched.error, fetched.response);
}

export function fieldIssues(error: unknown): FieldIssue[] {
  if (!(error instanceof ApiProblem)) return [];
  return Object.entries(error.problem.errors ?? {}).flatMap(([field, errors]) =>
    errors.map((error) => ({ field, ...error }))
  );
}

export function applyServerFieldErrors<Key extends string>(
  error: unknown,
  fields: Readonly<Record<string, Key>>
): Partial<Record<Key, string>> {
  const mapped: Partial<Record<Key, string>> = {};
  if (!(error instanceof ApiProblem)) return mapped;
  for (const [field, messages] of Object.entries(error.problem.errors ?? {})) {
    const local = fields[field];
    if (local && messages[0]) mapped[local] = messages[0].message;
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

export function serializeIfMatch(value: string): string {
  return BARE_UUID_ETAG.test(value) ? `"${value}"` : value;
}

export function abortError(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError';
}

export function unanswered(error: unknown): boolean {
  if (error instanceof ApiProblem)
    return [502, 503, 504].includes(error.problem.status);
  return error instanceof TypeError;
}

export function retryQuery(failureCount: number, error: unknown): boolean {
  if (failureCount >= 1) return false;
  if (abortError(error)) return false;
  return unanswered(error);
}
