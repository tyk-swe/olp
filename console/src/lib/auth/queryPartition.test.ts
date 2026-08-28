import { QueryClient } from '@tanstack/svelte-query';
import { describe, expect, it } from 'vitest';
import { QueryPartition } from './queryPartition';

describe('QueryPartition', () => {
  it('hashes keys under the current partition and changes on rotation', () => {
    const partition = new QueryPartition();
    const before = partition.keyHash(['providers']);
    partition.rotateAnonymous();
    const rotated = partition.keyHash(['providers']);
    partition.use('principal:1:owner');
    const principal = partition.keyHash(['providers']);
    expect(new Set([before, rotated, principal]).size).toBe(3);
    expect(partition.current()).toBe('principal:1:owner');
  });

  it('clears an attached client and forgets a detached one', async () => {
    const partition = new QueryPartition();
    const client = new QueryClient();
    client.setQueryData(['x'], 1);
    const detach = partition.attach(client);
    await partition.cancelAndClear();
    expect(client.getQueryData(['x'])).toBeUndefined();
    client.setQueryData(['x'], 2);
    detach();
    await partition.cancelAndClear();
    expect(client.getQueryData(['x'])).toBe(2);
  });
});
