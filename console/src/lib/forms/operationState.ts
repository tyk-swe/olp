import { ApiProblem } from '$lib/api/http';

export type OperationStatus =
  | 'idle'
  | 'in_flight'
  | 'indeterminate'
  | 'in_progress'
  | 'failed'
  | 'succeeded';

export type ErrorClassification = 'indeterminate' | 'in_progress' | 'definitive_failure';

/**
 * Classify API or transport errors according to the repository idempotency and error contract:
 * - definitive_failure: 4xx errors (e.g. 400, 401, 403, 404, 412, 422, 428, and non-in-progress 409)
 *   where the server rejected the request prior to execution or cannot proceed without corrections.
 * - in_progress: 409 with idempotency_in_progress, where the server is currently executing the mutation.
 * - indeterminate: network failure, dropped connection, abort, timeout, or 5xx server errors,
 *   where the mutation may have already committed on the server.
 */
export function classifyOperationError(error: unknown): ErrorClassification {
  if (error instanceof ApiProblem) {
    const status = error.problem.status;
    const problemType = error.problem.type ?? '';
    const title = error.problem.title ?? '';

    if (status === 409) {
      if (
        problemType.includes('idempotency_in_progress') ||
        title.toLowerCase().includes('in progress')
      ) {
        return 'in_progress';
      }
      return 'definitive_failure';
    }

    if (status >= 500) {
      return 'indeterminate';
    }

    return 'definitive_failure';
  }

  // Fetch transport failures (TypeError: Failed to fetch, DOMException, network drops, etc.)
  return 'indeterminate';
}

export function formatOperationErrorMessage(
  error: unknown,
  classification: ErrorClassification,
  fallback = 'The control API could not complete the request.'
): string {
  const baseMessage = error instanceof Error ? error.message : fallback;

  if (classification === 'indeterminate') {
    return `Outcome unknown — ${baseMessage} Retry safely to verify or complete the operation.`;
  }
  if (classification === 'in_progress') {
    return `Operation in progress — ${baseMessage} Retry safely to check for completion.`;
  }
  return baseMessage;
}

function deepClone<T>(value: T): T {
  if (value === undefined || value === null || typeof value !== 'object') {
    return value;
  }
  if (typeof structuredClone === 'function') {
    try {
      return structuredClone(value);
    } catch {
      // Fall through to JSON clone if structuredClone fails on simple records
    }
  }
  return JSON.parse(JSON.stringify(value));
}

function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a === undefined || b === undefined || a === null || b === null) return false;
  return JSON.stringify(a) === JSON.stringify(b);
}

/**
 * Controller enforcing the logical user operation lifecycle:
 * «One user-confirmed logical mutation owns one idempotency key and one immutable semantic
 * request until the operation reaches a definitive terminal outcome or is explicitly abandoned.»
 */
export class LogicalOperation<TPayload = void, TResult = void> {
  private _idempotencyKey: string | null = null;
  private _payload: TPayload | null = null;
  private _status: OperationStatus = 'idle';
  private _error: unknown | null = null;
  private _errorMessage: string = '';
  private readonly _action: (payload: TPayload, idempotencyKey: string) => Promise<TResult>;

  constructor(action: (payload: TPayload, idempotencyKey: string) => Promise<TResult>) {
    this._action = action;
  }

  get status(): OperationStatus {
    return this._status;
  }

  get idempotencyKey(): string | null {
    return this._idempotencyKey;
  }

  get payload(): TPayload | null {
    return this._payload;
  }

  get error(): unknown | null {
    return this._error;
  }

  get errorMessage(): string {
    return this._errorMessage;
  }

  get isIndeterminate(): boolean {
    return this._status === 'indeterminate' || this._status === 'in_progress';
  }

  get isBusy(): boolean {
    return this._status === 'in_flight';
  }

  get canRetry(): boolean {
    return this.isIndeterminate && this._idempotencyKey !== null;
  }

  /**
   * Execute the logical operation.
   * - If retrying an indeterminate or in-progress operation, re-uses the retained key and immutable payload.
   * - If a different payload is supplied while an indeterminate operation is retained, throws an error.
   * - If starting a new operation, generates a fresh UUID idempotency key and captures an immutable payload snapshot.
   */
  async execute(payload?: TPayload): Promise<TResult> {
    if (this._status === 'in_flight') {
      throw new Error('Operation is already in flight.');
    }

    let key: string;
    let targetPayload: TPayload;

    if (this.canRetry) {
      if (
        payload !== undefined &&
        this._payload !== null &&
        !deepEqual(payload, this._payload)
      ) {
        throw new Error(
          'Cannot modify request parameters for an operation with an indeterminate outcome. ' +
            'Retry with the original parameters or explicitly abandon the operation.'
        );
      }
      key = this._idempotencyKey!;
      targetPayload = this._payload as TPayload;
    } else {
      key = crypto.randomUUID();
      targetPayload = deepClone(payload as TPayload);
      this._idempotencyKey = key;
      this._payload = targetPayload;
    }

    this._status = 'in_flight';
    this._error = null;
    this._errorMessage = '';

    try {
      const result = await this._action(targetPayload, key);
      this._status = 'succeeded';
      this._idempotencyKey = null;
      this._payload = null;
      return result;
    } catch (err) {
      const classification = classifyOperationError(err);
      this._error = err;
      this._errorMessage = formatOperationErrorMessage(err, classification);

      if (classification === 'indeterminate') {
        this._status = 'indeterminate';
      } else if (classification === 'in_progress') {
        this._status = 'in_progress';
      } else {
        this._status = 'failed';
        this._idempotencyKey = null;
        this._payload = null;
      }
      throw err;
    }
  }

  /**
   * Retry the retained operation using its existing idempotency key and unchanged payload.
   */
  async retry(): Promise<TResult> {
    if (!this.canRetry) {
      throw new Error('No retained operation available to retry.');
    }
    return this.execute(this._payload as TPayload);
  }

  /**
   * Explicitly abandon any retained operation, discarding the key and frozen payload.
   */
  abandon(): void {
    this._idempotencyKey = null;
    this._payload = null;
    this._status = 'idle';
    this._error = null;
    this._errorMessage = '';
  }
}

/**
 * Manage a collection of logical operations keyed by resource ID or action name.
 */
export class KeyedLogicalOperations<TKey, TPayload = void, TResult = void> {
  private readonly _operations = new Map<TKey, LogicalOperation<TPayload, TResult>>();
  private readonly _factory: (key: TKey) => (payload: TPayload, idempotencyKey: string) => Promise<TResult>;

  constructor(
    factory: (key: TKey) => (payload: TPayload, idempotencyKey: string) => Promise<TResult>
  ) {
    this._factory = factory;
  }

  get(key: TKey): LogicalOperation<TPayload, TResult> {
    let op = this._operations.get(key);
    if (!op) {
      op = new LogicalOperation<TPayload, TResult>(this._factory(key));
      this._operations.set(key, op);
    }
    return op;
  }

  async execute(key: TKey, payload?: TPayload): Promise<TResult> {
    return this.get(key).execute(payload);
  }

  async retry(key: TKey): Promise<TResult> {
    return this.get(key).retry();
  }

  abandon(key: TKey): void {
    this._operations.get(key)?.abandon();
  }

  clear(): void {
    for (const op of this._operations.values()) {
      op.abandon();
    }
    this._operations.clear();
  }
}
