import { describe, expect, it } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import {
  classifyOperationError,
  formatOperationErrorMessage,
  KeyedLogicalOperations,
  LogicalOperation
} from './operationState';

describe('classifyOperationError', () => {
  it('classifies network errors and unhandled exceptions as indeterminate', () => {
    expect(classifyOperationError(new TypeError('Failed to fetch'))).toBe('indeterminate');
    expect(classifyOperationError(new DOMException('The operation was aborted'))).toBe('indeterminate');
    expect(classifyOperationError(new Error('Connection reset'))).toBe('indeterminate');
  });

  it('classifies 5xx server errors as indeterminate', () => {
    expect(
      classifyOperationError(
        new ApiProblem({ status: 500, title: 'Internal Server Error' })
      )
    ).toBe('indeterminate');
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 503,
          title: 'Database Unavailable',
          type: 'https://openllmproxy.dev/problems/database_unavailable'
        })
      )
    ).toBe('indeterminate');
    expect(
      classifyOperationError(
        new ApiProblem({ status: 502, title: 'Bad Gateway' })
      )
    ).toBe('indeterminate');
  });

  it('classifies 409 idempotency_in_progress as in_progress', () => {
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 409,
          title: 'Idempotency in progress',
          type: 'https://openllmproxy.dev/problems/idempotency_in_progress'
        })
      )
    ).toBe('in_progress');
  });

  it('classifies other 4xx errors as definitive failure', () => {
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 400,
          title: 'Bad Request',
          type: 'https://openllmproxy.dev/problems/bad_request'
        })
      )
    ).toBe('definitive_failure');
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 412,
          title: 'Precondition Failed',
          type: 'https://openllmproxy.dev/problems/etag_mismatch'
        })
      )
    ).toBe('definitive_failure');
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 422,
          title: 'Validation Failed',
          type: 'https://openllmproxy.dev/problems/validation_failed'
        })
      )
    ).toBe('definitive_failure');
    expect(
      classifyOperationError(
        new ApiProblem({
          status: 409,
          title: 'Idempotency key reused',
          type: 'https://openllmproxy.dev/problems/idempotency_key_reused'
        })
      )
    ).toBe('definitive_failure');
  });
});

describe('formatOperationErrorMessage', () => {
  it('formats indeterminate errors with safe retry guidance', () => {
    const message = formatOperationErrorMessage(
      new Error('Network disconnected'),
      'indeterminate'
    );
    expect(message).toContain('Outcome unknown');
    expect(message).toContain('Network disconnected');
    expect(message).toContain('Retry safely');
  });

  it('formats in_progress errors with check retry guidance', () => {
    const message = formatOperationErrorMessage(
      new Error('Operation in progress'),
      'in_progress'
    );
    expect(message).toContain('Operation in progress');
    expect(message).toContain('Retry safely');
  });

  it('formats definitive failures as standard messages', () => {
    const message = formatOperationErrorMessage(
      new Error('Invalid name'),
      'definitive_failure'
    );
    expect(message).toBe('Invalid name');
  });
});

describe('LogicalOperation lifecycle and invariants', () => {
  it('(1) retrying one logical operation sends same key', async () => {
    const keys: string[] = [];
    let attempts = 0;
    const op = new LogicalOperation<{ name: string }, { id: string }>(
      async (payload, idempotencyKey) => {
        keys.push(idempotencyKey);
        attempts += 1;
        if (attempts === 1) {
          throw new TypeError('Failed to fetch');
        }
        return { id: 'key-1' };
      }
    );

    await expect(op.execute({ name: 'my-key' })).rejects.toThrow('Failed to fetch');
    expect(op.isIndeterminate).toBe(true);
    expect(op.canRetry).toBe(true);
    expect(op.idempotencyKey).toBe(keys[0]);

    const result = await op.retry();
    expect(result).toEqual({ id: 'key-1' });
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBe(keys[1]);
    expect(op.status).toBe('succeeded');
    expect(op.idempotencyKey).toBeNull();
  });

  it('(2) a new logical operation gets a new key', async () => {
    const keys: string[] = [];
    const op = new LogicalOperation<{ name: string }, { id: string }>(
      async (payload, idempotencyKey) => {
        keys.push(idempotencyKey);
        return { id: `key-${keys.length}` };
      }
    );

    const first = await op.execute({ name: 'first' });
    const second = await op.execute({ name: 'second' });

    expect(first.id).toBe('key-1');
    expect(second.id).toBe('key-2');
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  it('(3) body, path inputs, and ETag cannot change under a retained key', async () => {
    let attempts = 0;
    const op = new LogicalOperation<{ name: string; limits: number }, void>(
      async () => {
        attempts += 1;
        if (attempts === 1) {
          throw new Error('503 Service Unavailable');
        }
      }
    );

    await expect(op.execute({ name: 'initial', limits: 10 })).rejects.toThrow();
    expect(op.isIndeterminate).toBe(true);

    // Attempting to execute with a different payload must be rejected
    await expect(op.execute({ name: 'modified', limits: 20 })).rejects.toThrow(
      'Cannot modify request parameters for an operation with an indeterminate outcome'
    );

    // Explicitly abandoning allows starting a new operation with the modified input
    op.abandon();
    expect(op.status).toBe('idle');
    expect(op.idempotencyKey).toBeNull();
    await expect(op.execute({ name: 'modified', limits: 20 })).resolves.toBeUndefined();
  });

  it('(4) definitive validation or precondition failure permits a corrected new operation with a new key', async () => {
    const keys: string[] = [];
    let attempts = 0;
    const op = new LogicalOperation<{ name: string }, { id: string }>(
      async (payload, idempotencyKey) => {
        keys.push(idempotencyKey);
        attempts += 1;
        if (attempts === 1) {
          throw new ApiProblem({
            status: 422,
            title: 'Validation failed',
            type: 'https://openllmproxy.dev/problems/validation_failed'
          });
        }
        return { id: 'success' };
      }
    );

    await expect(op.execute({ name: 'invalid-name' })).rejects.toThrow();
    expect(op.status).toBe('failed');
    expect(op.isIndeterminate).toBe(false);
    expect(op.idempotencyKey).toBeNull();

    // Now correcting the input produces a new operation with a brand new key
    const result = await op.execute({ name: 'valid-name' });
    expect(result).toEqual({ id: 'success' });
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  it('(5) in-progress retry preserves the same key', async () => {
    const keys: string[] = [];
    let attempts = 0;
    const op = new LogicalOperation<{ name: string }, { id: string }>(
      async (payload, idempotencyKey) => {
        keys.push(idempotencyKey);
        attempts += 1;
        if (attempts === 1) {
          throw new ApiProblem({
            status: 409,
            title: 'Idempotency in progress',
            type: 'https://openllmproxy.dev/problems/idempotency_in_progress'
          });
        }
        return { id: 'done' };
      }
    );

    await expect(op.execute({ name: 'test' })).rejects.toThrow();
    expect(op.status).toBe('in_progress');
    expect(op.isIndeterminate).toBe(true);

    const result = await op.retry();
    expect(result).toEqual({ id: 'done' });
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBe(keys[1]);
  });

  it('deeply clones and isolates payload from subsequent caller mutation', async () => {
    const receivedPayloads: Array<{ nested: { count: number } }> = [];
    const op = new LogicalOperation<{ nested: { count: number } }, void>(
      async (payload) => {
        receivedPayloads.push(payload);
        if (receivedPayloads.length === 1) {
          throw new Error('500 server error');
        }
      }
    );

    const input = { nested: { count: 1 } };
    await expect(op.execute(input)).rejects.toThrow();

    // Caller mutates original object
    input.nested.count = 999;

    // Retry must use the originally captured snapshot (count: 1)
    await op.retry();
    expect(receivedPayloads[0]!.nested.count).toBe(1);
    expect(receivedPayloads[1]!.nested.count).toBe(1);
  });
});

describe('KeyedLogicalOperations', () => {
  it('manages independent operation lifecycles by key', async () => {
    const calls: Record<string, string[]> = { 'key-1': [], 'key-2': [] };
    const keyed = new KeyedLogicalOperations<string, { role: string }, void>(
      (id) => async (payload, idempotencyKey) => {
        calls[id]!.push(idempotencyKey);
        if (id === 'key-1' && calls[id]!.length === 1) {
          throw new TypeError('Network error');
        }
      }
    );

    await expect(keyed.execute('key-1', { role: 'admin' })).rejects.toThrow();
    expect(keyed.get('key-1').isIndeterminate).toBe(true);

    await expect(keyed.execute('key-2', { role: 'user' })).resolves.toBeUndefined();
    expect(keyed.get('key-2').status).toBe('succeeded');

    // Retrying key-1 preserves key-1's idempotency key
    await keyed.retry('key-1');
    expect(calls['key-1']![0]).toBe(calls['key-1']![1]);
    expect(calls['key-1']![0]).not.toBe(calls['key-2']![0]);
  });
});
