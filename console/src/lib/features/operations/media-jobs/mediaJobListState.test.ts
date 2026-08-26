import { describe, expect, it } from 'vitest';
import {
  emptyMediaJobListState,
  mediaJobFilters,
  type MediaJobListState
} from './mediaJobListState';

function state(changes: Partial<MediaJobListState> = {}): MediaJobListState {
  return { ...emptyMediaJobListState(), ...changes };
}

describe('mediaJobFilters', () => {
  it('sends only the page size when nothing is filled in', () => {
    expect(mediaJobFilters(state())).toEqual({
      limit: 25,
      route: undefined,
      state: undefined,
      lifecycle: undefined,
      api_key_id: undefined,
      provider_id: undefined,
      created_after: undefined,
      created_before: undefined
    });
  });

  it('maps every field to its query parameter and trims identifiers', () => {
    expect(
      mediaJobFilters(
        state({
          route: ' video-render ',
          jobState: 'running',
          lifecycle: 'create_ambiguous',
          apiKeyId: ' 01980000-0000-7000-8000-000000000103 ',
          providerId: '01980000-0000-7000-8000-000000000104'
        })
      )
    ).toMatchObject({
      route: 'video-render',
      state: 'running',
      lifecycle: 'create_ambiguous',
      api_key_id: '01980000-0000-7000-8000-000000000103',
      provider_id: '01980000-0000-7000-8000-000000000104'
    });
  });

  it('converts local date bounds to instants', () => {
    const filters = mediaJobFilters(
      state({ createdAfter: '2026-07-12T09:30', createdBefore: '2026-07-12T18:00' })
    );
    expect(filters.created_after).toBe(new Date('2026-07-12T09:30').toISOString());
    expect(filters.created_before).toBe(new Date('2026-07-12T18:00').toISOString());
  });

  it('drops a half-typed date instead of sending an invalid bound', () => {
    expect(mediaJobFilters(state({ createdAfter: '2026-13-45T99:99' })).created_after).toBeUndefined();
    expect(mediaJobFilters(state({ createdBefore: '   ' })).created_before).toBeUndefined();
  });
});
