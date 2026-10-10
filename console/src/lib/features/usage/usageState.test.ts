import { describe, expect, it } from 'vitest';
import { dateTimeLocalValue } from '$lib/format';
import {
  applyUsageDraft,
  clearedUsageState,
  defaultUsageState,
  readUsageState,
  usageDraft,
  usageProblem,
  usageSearch
} from '$lib/features/usage/usageState';

const defaults = defaultUsageState(new Date('2026-07-12T12:00:35.123Z'));
const key = '01980000-0000-7000-8000-000000000103';

describe('usage report URL state', () => {
  it('preserves case-sensitive session identifiers through URLs and draft edits', () => {
    const state = readUsageState(
      new URLSearchParams({
        session_id: 'SessionCase/Alpha',
        attribution_key: 'session',
        attribution_value: 'SessionCase/Alpha',
        project_id: '01980000-0000-7000-8000-00000000ABCD',
        dimension: 'session'
      }),
      defaults
    );
    expect(state.filters.session_id).toBe('SessionCase/Alpha');
    expect(state.filters.project_id).toBe(
      '01980000-0000-7000-8000-00000000abcd'
    );
    expect(usageProblem(state)).toBeNull();
    expect(
      readUsageState(new URLSearchParams(usageSearch(state)), defaults)
    ).toEqual(state);
    expect(applyUsageDraft(usageDraft(state), state)).toEqual(state);
  });

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
    ['api_key_id=invalid', 'API key ID must be a UUID.'],
    [
      'attribution_value=core',
      'An attribution value needs its attribution key.'
    ],
    [
      'dimension=attribution',
      'The attribution breakdown needs an attribution key.'
    ]
  ])(
    'retains invalid URL state for visible validation: %s',
    (search, message) => {
      expect(
        usageProblem(readUsageState(new URLSearchParams(search), defaults))
      ).toBe(message);
    }
  );

  it('round trips attribution filters and the attribution breakdown axis', () => {
    const state = readUsageState(
      new URLSearchParams(
        'attribution_key=team&attribution_value=core&dimension=attribution'
      ),
      defaults
    );
    expect(state.filters.attribution_key).toBe('team');
    expect(state.filters.attribution_value).toBe('core');
    expect(state.dimension).toBe('attribution');
    expect(usageProblem(state)).toBeNull();
    const canonical = usageSearch(state);
    expect(canonical).toContain('attribution_key=team');
    expect(canonical).toContain('attribution_value=core');
    expect(canonical).toContain('dimension=attribution');
    expect(readUsageState(new URLSearchParams(canonical), defaults)).toEqual(
      state
    );
  });

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

  it.each([
    '2026-02-30T12:00:00Z',
    '2027-02-29T12:00:00Z',
    '2026-03-01T24:00:00Z'
  ])('keeps impossible calendar instants invalid in the URL: %s', (start) => {
    const state = readUsageState(new URLSearchParams({ start }), defaults);
    expect(state.filters.start).toBe(start);
    expect(usageProblem(state)).toBe('Enter valid start and end times.');
    expect(usageSearch(state)).toContain(`start=${encodeURIComponent(start)}`);
  });

  it.each([
    {
      bound: 'start',
      invalid: '2026-02-30T12:00:00Z',
      other: '2026-03-10T12:00:00Z'
    },
    {
      bound: 'start',
      invalid: '2027-02-29T12:00:00Z',
      other: '2027-03-10T12:00:00Z'
    },
    {
      bound: 'start',
      invalid: '2026-03-01T24:00:00Z',
      other: '2026-03-10T12:00:00Z'
    },
    {
      bound: 'end',
      invalid: '2026-02-30T12:00:00Z',
      other: '2026-02-01T12:00:00Z'
    },
    {
      bound: 'end',
      invalid: '2027-02-29T12:00:00Z',
      other: '2027-02-01T12:00:00Z'
    },
    {
      bound: 'end',
      invalid: '2026-03-01T24:00:00Z',
      other: '2026-02-01T12:00:00Z'
    }
  ] as const)(
    'normalizes the corrected visible $bound $invalid when its draft is applied',
    ({ bound, invalid, other }) => {
      const otherBound = bound === 'start' ? 'end' : 'start';
      const canonical = new Date(other).toISOString();
      const state = readUsageState(
        new URLSearchParams({ [bound]: invalid, [otherBound]: other }),
        defaults
      );
      expect(state.filters[bound]).toBe(invalid);
      expect(state.filters[otherBound]).toBe(canonical);
      expect(usageProblem(state)).toBe('Enter valid start and end times.');
      const corrected = new Date(invalid).toISOString();
      const draft = usageDraft(state);
      expect(draft[bound]).toBe(dateTimeLocalValue(corrected));

      const applied = applyUsageDraft(draft, state);
      expect(applied.filters[bound]).toBe(corrected);
      expect(applied.filters[otherBound]).toBe(canonical);
      expect(usageProblem(applied)).toBeNull();

      const changed = applyUsageDraft(
        { ...draft, route: 'changed-route' },
        state
      );
      expect(changed.filters[bound]).toBe(corrected);
      expect(changed.filters[otherBound]).toBe(canonical);
      expect(changed.filters.route).toBe('changed-route');
      expect(usageProblem(changed)).toBeNull();
    }
  );

  it('keeps impossible local calendar dates invalid in drafts', () => {
    for (const start of ['2026-02-30T12:00', '2027-02-29T12:00']) {
      const draft = { ...usageDraft(defaults), start };
      const applied = applyUsageDraft(draft, defaults);
      expect(applied.filters.start).toBe(start);
      expect(usageProblem(applied)).toBe('Enter valid start and end times.');
    }
  });

  it('accepts real leap days and canonicalizes them to UTC', () => {
    const state = readUsageState(
      new URLSearchParams({
        start: '2028-02-29T12:00:00Z',
        end: '2028-03-01T00:00:00Z'
      }),
      defaults
    );
    expect(state.filters.start).toBe('2028-02-29T12:00:00.000Z');
    expect(usageProblem(state)).toBeNull();
  });

  it.each([
    ['2026-03-01T12:00:00.1Z', '2026-03-01T12:00:00.100Z'],
    ['2026-03-01T12:00:00.12Z', '2026-03-01T12:00:00.120Z'],
    ['2026-03-01T12:00:00.0001Z', '2026-03-01T12:00:00.0001Z'],
    ['2026-03-01T12:00:00.12345Z', '2026-03-01T12:00:00.12345Z'],
    ['2026-03-01T12:00:00.123456Z', '2026-03-01T12:00:00.123456Z'],
    ['2026-03-01T12:00:00.1234567Z', '2026-03-01T12:00:00.1234567Z'],
    ['2026-03-01T12:00:00.12345678Z', '2026-03-01T12:00:00.12345678Z'],
    ['2026-03-01T12:00:00.123456789Z', '2026-03-01T12:00:00.123456789Z'],
    ['2026-03-01T07:00:00.123456789-05:00', '2026-03-01T12:00:00.123456789Z'],
    ['2026-03-01T13:30:00.987654321+01:30', '2026-03-01T12:00:00.987654321Z']
  ])(
    'round trips %s through the URL without losing precision',
    (start, expected) => {
      const state = readUsageState(
        new URLSearchParams({ start, end: '2026-03-02T00:00:00Z' }),
        defaults
      );
      expect(state.filters.start).toBe(expected);
      expect(usageProblem(state)).toBeNull();
      const canonical = usageSearch(state);
      expect(canonical).toContain(`start=${encodeURIComponent(expected)}`);
      expect(
        readUsageState(new URLSearchParams(canonical), defaults).filters.start
      ).toBe(expected);
    }
  );

  it('orders bounds at sub-millisecond precision', () => {
    expect(
      usageProblem(
        readUsageState(
          new URLSearchParams({
            start: '2026-03-01T12:00:00.000000001Z',
            end: '2026-03-01T12:00:00.000000002Z'
          }),
          defaults
        )
      )
    ).toBeNull();
    for (const [start, end] of [
      ['2026-03-01T12:00:00.000000001Z', '2026-03-01T12:00:00.000000001Z'],
      ['2026-03-01T12:00:00.000000002Z', '2026-03-01T12:00:00.000000001Z']
    ]) {
      expect(
        usageProblem(
          readUsageState(new URLSearchParams({ start, end }), defaults)
        )
      ).toBe('End must be after start.');
    }
  });

  it('keeps sub-millisecond precision on a changed local draft bound', () => {
    const draft = {
      ...usageDraft(defaults),
      start: '2026-07-11T08:00:00.12345'
    };
    const applied = applyUsageDraft(draft, defaults);
    expect(applied.filters.start).toBe('2026-07-11T12:00:00.12345Z');
    expect(usageProblem(applied)).toBeNull();
  });

  it('preserves exact bounds when another filter changes', () => {
    const state = readUsageState(
      new URLSearchParams({
        start: '2026-03-01T12:00:00.000000001Z',
        end: '2026-03-02T12:00:00.000000009Z'
      }),
      defaults
    );
    const updated = applyUsageDraft(
      { ...usageDraft(state), route: 'exact-route' },
      state
    );
    expect(updated.filters.start).toBe('2026-03-01T12:00:00.000000001Z');
    expect(updated.filters.end).toBe('2026-03-02T12:00:00.000000009Z');
    expect(updated.filters.route).toBe('exact-route');
  });
});

describe('clearing the usage report', () => {
  it('clears to a valid state when the breakdown is by attribution', () => {
    const now = new Date('2026-07-12T12:00:35.123Z');
    const state = clearedUsageState(
      { dimension: 'attribution', granularity: 'day' },
      now
    );
    expect(state.dimension).toBe('route');
    expect(state.granularity).toBe('day');
    expect(state.filters.attribution_key).toBeUndefined();
    expect(usageProblem(state)).toBeNull();
    expect(
      usageProblem(
        readUsageState(new URLSearchParams(usageSearch(state)), defaults)
      )
    ).toBeNull();
  });

  it('keeps a breakdown that needs no filter', () => {
    expect(
      clearedUsageState({ dimension: 'model', granularity: 'hour' }).dimension
    ).toBe('model');
    for (const dimension of ['model_family', 'estimate_provenance'] as const) {
      expect(
        clearedUsageState({ dimension, granularity: 'hour' }).dimension
      ).toBe(dimension);
    }
  });

  it('round trips the estimate breakdowns through the URL', () => {
    for (const dimension of ['model_family', 'estimate_provenance'] as const) {
      const state = readUsageState(
        new URLSearchParams({ dimension }),
        defaults
      );
      expect(state.dimension).toBe(dimension);
      expect(usageProblem(state)).toBeNull();
      expect(usageSearch(state)).toContain(`dimension=${dimension}`);
      expect(
        readUsageState(new URLSearchParams(usageSearch(state)), defaults)
      ).toEqual(state);
    }
  });
});
