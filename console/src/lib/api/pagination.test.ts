import { describe, expect, it, vi } from 'vitest';
import { collectCursorPages } from './pagination';

describe('collectCursorPages', () => {
  it('collects every page and sends each cursor once', async () => {
    const load = vi.fn(async (cursor?: string) =>
      cursor
        ? { items: ['second'], nextCursor: null }
        : { items: ['first'], nextCursor: 'page-2' }
    );

    await expect(collectCursorPages(load)).resolves.toEqual([
      'first',
      'second'
    ]);
    expect(load.mock.calls).toEqual([[undefined], ['page-2']]);
  });

  it('rejects a cursor cycle before requesting a page twice', async () => {
    const load = vi.fn(async (cursor?: string) => ({
      items: [],
      nextCursor: cursor === 'page-2' ? 'page-1' : 'page-2'
    }));

    await expect(collectCursorPages(load)).rejects.toMatchObject({
      problem: {
        type: 'urn:olp:problem:invalid-cursor-cycle',
        status: 502
      }
    });
    expect(load.mock.calls).toEqual([[undefined], ['page-2'], ['page-1']]);
  });

  it('accepts exactly 10,000 items and rejects the next item without another fetch', async () => {
    const boundary = Array(10_000).fill(0);
    await expect(
      collectCursorPages(async () => ({ items: boundary, nextCursor: null }))
    ).resolves.toHaveLength(10_000);

    const load = vi.fn(async (cursor?: string) =>
      cursor
        ? { items: [1], nextCursor: 'page-3' }
        : { items: boundary, nextCursor: 'page-2' }
    );
    await expect(collectCursorPages(load)).rejects.toMatchObject({
      problem: {
        type: 'urn:olp:problem:pagination-limit-exceeded',
        status: 502
      }
    });
    expect(load.mock.calls).toEqual([[undefined], ['page-2']]);
  });
});
