import { beforeEach, expect, it } from 'vitest';
import { filterSearch, readSavedFilters, saveFilters } from './savedFilters';

const first = '00000000-0000-4000-8000-000000000001';
const second = '00000000-0000-4000-8000-000000000002';
beforeEach(() => sessionStorage.clear());

it('restores allowed filters only inside the same session and feature', () => {
  saveFilters(sessionStorage, first, 'usage', [
    {
      name: 'My route',
      search:
        '?route=assistant&token=secret&cursor=old&start=2026-10-01T00%3A00%3A00Z'
    }
  ]);
  expect(readSavedFilters(sessionStorage, first, 'usage')).toEqual([
    {
      name: 'My route',
      search: '?start=2026-10-01T00%3A00%3A00Z&route=assistant'
    }
  ]);
  expect(readSavedFilters(sessionStorage, first, 'history')).toEqual([]);
  expect(
    sessionStorage.getItem(`olp.saved-filters.v1.${first}.usage`)
  ).not.toContain('secret');
  expect(readSavedFilters(sessionStorage, second, 'usage')).toEqual([]);
  expect(
    sessionStorage.getItem(`olp.saved-filters.v1.${first}.usage`)
  ).toBeNull();
});

it('drops paging and unknown query fields from history views', () => {
  expect(
    filterSearch(
      'history',
      '?route=assistant&status_code=503&cursor=next&csrf_token=secret'
    )
  ).toBe('?route=assistant&status_code=503');
});
