import { describe, expect, it } from 'vitest';
import {
  formTimeValue,
  preservedTime,
  querySearch,
  rangeProblem,
  readForm,
  timeInputType,
  timeOrder,
  timeProblem,
  timeValid,
  uuidProblem
} from './filters';

describe('time filters', () => {
  it.each([
    '2024-02-29T23:59',
    '2000-02-29T00:00',
    '2025-01-31T12:30:45Z',
    '2025-01-31T12:30:45.123456789+05:30'
  ])('accepts a real calendar time %s', (value) => {
    expect(timeValid(value)).toBe(true);
  });

  it.each([
    '2023-02-29T12:00',
    '1900-02-29T12:00',
    '2024-02-30T12:00',
    '2025-04-31T12:00',
    '2025-01-01T24:00',
    '2025-01-01T12:60',
    '2025-01-01T12:00Z',
    'not-a-time'
  ])('rejects an invalid calendar time %s', (value) => {
    expect(timeValid(value)).toBe(false);
  });

  it('requires a timezone for shared links but permits local form values', () => {
    expect(timeValid('2025-01-31T12:30')).toBe(true);
    expect(timeValid('2025-01-31T12:30', true)).toBe(false);
    expect(timeValid('2025-01-31T12:30:00Z', true)).toBe(true);
    expect(timeValid('2025-01-31T12:30:00-04:00', true)).toBe(true);
    expect(timeProblem([['2025-01-31T12:30', 'Start']], true)).toBe(
      'Start must be a valid time with a timezone: 2025-01-31T12:30'
    );
    expect(timeProblem([[' ', 'Start']], true)).toBeNull();
  });

  it('preserves precise applied instants when the local form has not changed', () => {
    const applied = '2025-03-01T15:02:10.123456789Z';
    const shown = formTimeValue(applied);

    expect(preservedTime(shown, applied)).toBe(applied);
    expect(preservedTime('2025-03-01T15:03:00z')).toBe('2025-03-01T15:03:00Z');
    expect(preservedTime('2025-03-01T15:03')).toBe(
      new Date('2025-03-01T15:03').toISOString()
    );
    expect(preservedTime('invalid')).toBeUndefined();
    expect(formTimeValue('invalid')).toBe('invalid');
  });

  it('keeps invalid or timezone-qualified text visible to the operator', () => {
    expect(timeInputType('')).toBe('datetime-local');
    expect(timeInputType('2025-01-31T12:30')).toBe('datetime-local');
    expect(timeInputType('2025-01-31T12:30:00Z')).toBe('text');
    expect(timeInputType('invalid')).toBe('text');
  });

  it('compares fractional seconds without rounding away nanoseconds', () => {
    const earlier = '2025-01-31T12:30:00.123456789Z';
    const later = '2025-01-31T12:30:00.123456790Z';
    const equivalent = '2025-01-31T07:30:00.123456789-05:00';

    expect(timeOrder(earlier)).toBe(timeOrder(equivalent));
    expect(timeOrder(earlier) < timeOrder(later)).toBe(true);
    expect(rangeProblem(earlier, later, 'Invalid range')).toBeNull();
    expect(rangeProblem(later, earlier, 'Invalid range')).toBe('Invalid range');
    expect(rangeProblem(earlier, earlier, 'Invalid range')).toBe(
      'Invalid range'
    );
    expect(rangeProblem(undefined, earlier, 'Invalid range')).toBeNull();
  });
});

describe('filter query values', () => {
  it('reports the first invalid nonempty identifier', () => {
    const id = '123e4567-e89b-12d3-a456-426614174000';

    expect(
      uuidProblem([
        [' ', 'Project'],
        [id.toUpperCase(), 'Provider']
      ])
    ).toBeNull();
    expect(
      uuidProblem([
        ['bad-id', 'Project'],
        ['also-bad', 'Provider']
      ])
    ).toBe('Project must be a UUID: bad-id');
  });

  it('reads trimmed fields and writes only selected, nonempty query values', () => {
    const search = new URLSearchParams(
      'route=%20demo%20&other=ignored&count=0'
    );
    const form = readForm(search, { route: 'route', missing: 'missing' });

    expect(form).toEqual({ route: 'demo', missing: '' });
    expect(
      querySearch({ route: form.route, count: 0, empty: '', skip: 'x' }, [
        'count',
        'route',
        'empty'
      ])
    ).toBe('count=0&route=demo');
  });
});
