import { describe, expect, it } from 'vitest';
import {
  applyUsageDraft,
  defaultUsageState,
  readUsageState,
  usageDraft,
  usageProblem,
  usageSearch
} from '$lib/features/usage/usageState';

const defaults = defaultUsageState(new Date('2026-07-12T12:00:35.123Z'));
const key = '01980000-0000-7000-8000-000000000103';

describe('usage report URL state', () => {
  it('anchors missing parameters and unknown axes to deterministic defaults', () => {
    const state = readUsageState(
      new URLSearchParams('dimension=unknown&granularity=minute'),
      defaults
    );
    expect(state).toEqual(defaults);
    expect(usageProblem(state)).toBeNull();
    expect(usageSearch(state)).toBe(
      'start=2026-07-11T12%3A00%3A35.123Z&end=2026-07-12T12%3A00%3A35.123Z&dimension=route&granularity=hour'
    );
  });

  it('round trips all applied metadata with normalized instants and no unrelated fields', () => {
    const search = new URLSearchParams({
      start: '2026-07-12T07:30:12-04:00',
      end: '2026-07-12T12:00:00Z',
      route: ' support/chat ',
      model: 'model & one',
      provider_id: key,
      api_key_id: key,
      operation: 'generation',
      dimension: 'api_key',
      granularity: 'day',
      unrelated: 'discard'
    });
    const state = readUsageState(search, defaults);
    const canonical = usageSearch(state);
    expect(readUsageState(new URLSearchParams(canonical), defaults)).toEqual(
      state
    );
    expect(state.filters.start).toBe('2026-07-12T11:30:12.000Z');
    expect(state.filters.route).toBe('support/chat');
    expect(state.filters.model).toBe('model & one');
    expect(canonical).not.toContain('unrelated');
    expect(usageProblem(state)).toBeNull();
  });

  it.each([
    ['start=not-a-date', 'Enter valid start and end times.'],
    ['start=2026-07-12T09:00:00', 'Enter valid start and end times.'],
    ['start=2026-07-13T12:00:00Z', 'End must be after start.'],
    ['provider_id=invalid', 'Provider ID must be a UUID.'],
    ['api_key_id=invalid', 'API key ID must be a UUID.']
  ])(
    'retains invalid URL state for visible validation: %s',
    (search, message) => {
      expect(
        usageProblem(readUsageState(new URLSearchParams(search), defaults))
      ).toBe(message);
    }
  );

  it('keeps draft edits separate and validates incomplete or inverted local ranges', () => {
    const draft = usageDraft(defaults);
    draft.route = 'changed';
    expect(defaults.filters.route).toBeUndefined();
    draft.start = '';
    expect(usageProblem(applyUsageDraft(draft, defaults))).toBe(
      'Enter valid start and end times.'
    );
    draft.start = '2026-07-15T08:00';
    expect(usageProblem(applyUsageDraft(draft, defaults))).toBe(
      'End must be after start.'
    );
  });

  it('converts changed local times using each side of the spring DST transition', () => {
    const draft = {
      ...usageDraft(defaults),
      start: '2026-03-08T01:30',
      end: '2026-03-08T03:30'
    };
    expect(applyUsageDraft(draft, defaults).filters).toEqual({
      start: '2026-03-08T06:30:00.000Z',
      end: '2026-03-08T07:30:00.000Z'
    });
  });

  it('preserves the second fall-back hour and subminute precision when times are unchanged', () => {
    const state = readUsageState(
      new URLSearchParams({
        start: '2026-11-01T06:30:42.123Z',
        end: '2026-11-01T07:30:12.456Z'
      }),
      defaults
    );
    const draft = usageDraft(state);
    expect(draft.start).toBe('2026-11-01T01:30');
    expect(draft.end).toBe('2026-11-01T02:30');
    draft.route = 'updated-filter';
    const updated = applyUsageDraft(draft, state);
    expect(updated.filters).toEqual({
      ...state.filters,
      route: 'updated-filter'
    });
  });
});
