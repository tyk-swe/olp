import { describe, expect, it } from 'vitest';
import {
  applyUsageDraft,
  clearedUsageState,
  defaultUsageState,
  readUsageState,
  usageDraft,
  usageProblem,
  usageSearch,
  type UsageState
} from './usageState';

const now = new Date('2025-04-02T12:00:00Z');
const defaultState = defaultUsageState(now);
const id = '123e4567-e89b-12d3-a456-426614174000';

describe('usage report filters', () => {
  it('defaults to the previous day and keeps the selected axes when cleared', () => {
    expect(new Date(defaultState.filters.end).valueOf()).toBe(now.valueOf());
    expect(
      new Date(defaultState.filters.end).valueOf() -
        new Date(defaultState.filters.start).valueOf()
    ).toBe(24 * 60 * 60 * 1000);
    expect(
      clearedUsageState({ dimension: 'provider', granularity: 'day' }, now)
    ).toEqual({ ...defaultState, dimension: 'provider', granularity: 'day' });
    expect(
      clearedUsageState({ dimension: 'attribution', granularity: 'day' }, now)
    ).toEqual({ ...defaultState, granularity: 'day' });
  });

  it('round trips a shared report link while normalizing IDs and preserving time precision', () => {
    const source = new URLSearchParams({
      start: '2025-04-01T08:00:00.123456789-04:00',
      end: '2025-04-02T08:00:00.987654321-04:00',
      provider_id: id.toUpperCase(),
      route: ' customer-route ',
      dimension: 'provider',
      granularity: 'day'
    });
    const state = readUsageState(source, defaultState);

    expect(state).toEqual({
      filters: {
        start: '2025-04-01T12:00:00.123456789Z',
        end: '2025-04-02T12:00:00.987654321Z',
        provider_id: id,
        route: 'customer-route'
      },
      dimension: 'provider',
      granularity: 'day'
    });
    expect(
      readUsageState(new URLSearchParams(usageSearch(state)), defaultState)
    ).toEqual(state);
    expect(usageProblem(state)).toBeNull();
  });

  it('preserves precise times when the operator applies an unchanged form', () => {
    const applied: UsageState = {
      ...defaultState,
      filters: {
        start: '2025-04-01T12:00:05.123456789Z',
        end: '2025-04-02T12:00:07.987654321Z',
        route: 'example'
      }
    };
    const draft = usageDraft(applied);

    expect(applyUsageDraft(draft, applied)).toEqual(applied);
    const changed = applyUsageDraft(
      { ...draft, start: '2025-04-01T10:30' },
      applied
    );
    expect(changed.filters.start).toBe(
      new Date('2025-04-01T10:30').toISOString()
    );
    expect(changed.filters.end).toBe(applied.filters.end);
  });

  it('retains invalid link input so the user can correct it', () => {
    const state = readUsageState(
      new URLSearchParams({
        start: 'invalid',
        dimension: 'unknown',
        granularity: 'unknown'
      }),
      defaultState
    );

    expect(state.filters.start).toBe('invalid');
    expect(state.filters.end).toBe(defaultState.filters.end);
    expect(state.dimension).toBe('route');
    expect(state.granularity).toBe('hour');
    expect(usageProblem(state)).toBe('Enter valid start and end times.');
  });

  it.each([
    { fields: { start: 'invalid' }, error: 'Enter valid start and end times.' },
    {
      fields: { start: defaultState.filters.end },
      error: 'End must be after start.'
    },
    { fields: { provider_id: 'bad' }, error: 'Provider ID must be a UUID.' },
    { fields: { api_key_id: 'bad' }, error: 'API key ID must be a UUID.' },
    { fields: { project_id: 'bad' }, error: 'Project ID must be a UUID.' },
    {
      fields: { attribution_value: 'team' },
      error: 'An attribution value needs its attribution key.'
    },
    {
      fields: { session_id: 'one', attribution_key: 'team' },
      error: 'The session filter conflicts with the attribution key.'
    },
    {
      fields: { session_id: 'one', attribution_value: 'two' },
      error: 'The session filter conflicts with the attribution value.'
    }
  ])('rejects invalid filters: $error', ({ fields, error }) => {
    expect(
      usageProblem({
        ...defaultState,
        filters: { ...defaultState.filters, ...fields }
      })
    ).toBe(error);
  });

  it('requires an attribution key for an attribution breakdown', () => {
    expect(usageProblem({ ...defaultState, dimension: 'attribution' })).toBe(
      'The attribution breakdown needs an attribution key.'
    );
    expect(
      usageProblem({
        ...defaultState,
        dimension: 'attribution',
        filters: {
          ...defaultState.filters,
          attribution_key: 'team',
          attribution_value: 'docs'
        }
      })
    ).toBeNull();
  });
});
