import { describe, expect, it, vi } from 'vitest';
import { filterSearch, readSavedFilters, saveFilters } from './savedFilters';

const firstSession = '123e4567-e89b-12d3-a456-426614174000';
const secondSession = '123e4567-e89b-12d3-a456-426614174001';

function storage(keys: string[] = []) {
  return {
    length: keys.length,
    clear: vi.fn(),
    getItem: vi.fn<(key: string) => string | null>(() => null),
    key: vi.fn((index: number) => keys[index] ?? null),
    removeItem: vi.fn(),
    setItem: vi.fn<(key: string, value: string) => void>()
  } satisfies Storage;
}

describe('saved usage and history views', () => {
  it('keeps only filters supported by each report and drops unrelated query values', () => {
    const search =
      '?route=demo&status_code=500&start=2025-01-01&cursor=next&secret=hidden';

    expect(filterSearch('usage', search)).toBe('?start=2025-01-01&route=demo');
    expect(filterSearch('history', search)).toBe('?route=demo&status_code=500');
    expect(filterSearch('usage', '?cursor=next')).toBe('');
  });

  it('saves normalized views for one session and report and reads them back', () => {
    const store = storage();
    saveFilters(store, firstSession, 'usage', [
      { name: '  Overview  ', search: '?route=demo&cursor=next' }
    ]);
    const [key, value] = store.setItem.mock.calls[0];
    store.getItem.mockImplementation((requested) =>
      requested === key ? value : null
    );

    expect(readSavedFilters(store, firstSession, 'usage')).toEqual([
      { name: 'Overview', search: '?route=demo' }
    ]);
    expect(readSavedFilters(store, firstSession, 'history')).toEqual([]);
    expect(readSavedFilters(store, secondSession, 'usage')).toEqual([]);
  });

  it('removes saved views from older sessions without touching unrelated data', () => {
    const first = storage();
    const current = storage();
    saveFilters(first, firstSession, 'usage', []);
    saveFilters(current, secondSession, 'usage', []);
    const oldKey = first.setItem.mock.calls[0][0];
    const currentKey = current.setItem.mock.calls[0][0];
    const store = storage([oldKey, currentKey, 'unrelated']);

    readSavedFilters(store, secondSession, 'usage');

    expect(store.removeItem).toHaveBeenCalledOnce();
    expect(store.removeItem).toHaveBeenCalledWith(oldKey);
  });

  it('ignores malformed, duplicate and oversized stored entries', () => {
    const store = storage();
    store.getItem.mockReturnValue(
      JSON.stringify([
        { name: '  Recent ', search: '?route=demo&cursor=next' },
        { name: 'Recent', search: '?route=other' },
        { name: '', search: '?route=empty' },
        { name: 'x'.repeat(61), search: '' },
        { name: 'Long query', search: 'x'.repeat(4097) },
        { name: 'Missing search' },
        null
      ])
    );

    expect(readSavedFilters(store, firstSession, 'history')).toEqual([
      { name: 'Recent', search: '?route=demo' }
    ]);
    store.getItem.mockReturnValue('{invalid');
    expect(readSavedFilters(store, firstSession, 'history')).toEqual([]);
    store.getItem.mockReturnValue('{}');
    expect(readSavedFilters(store, firstSession, 'history')).toEqual([]);
  });

  it('bounds the number of stored views', () => {
    const store = storage();
    const views = Array.from({ length: 21 }, (_, index) => ({
      name: `View ${index}`,
      search: ''
    }));
    store.getItem.mockReturnValue(JSON.stringify(views));

    expect(readSavedFilters(store, firstSession, 'usage')).toHaveLength(20);
    expect(() => saveFilters(store, firstSession, 'usage', views)).toThrow(
      'Keep at most 20 saved views per page.'
    );
    expect(store.setItem).not.toHaveBeenCalled();
  });

  it.each([
    { session: 'unverified', name: 'View', search: '' },
    { session: firstSession, name: '  ', search: '' },
    { session: firstSession, name: 'x'.repeat(61), search: '' },
    { session: firstSession, name: 'View', search: 'x'.repeat(4097) }
  ])(
    'does not save an invalid session or view',
    ({ session, name, search }) => {
      const store = storage();

      expect(() =>
        saveFilters(store, session, 'usage', [{ name, search }])
      ).toThrow();
      expect(store.setItem).not.toHaveBeenCalled();
    }
  );
});
