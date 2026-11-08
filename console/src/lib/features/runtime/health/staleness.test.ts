import { describe, expect, it } from 'vitest';
import {
  CHECKPOINT_STALE_SECONDS,
  PENDING_RECOVERY_SECONDS,
  ageStatus,
  formatAge,
  oldestPendingStatus,
  reportedAgeStatus,
  secondsSince
} from './staleness';

describe('checkpoint age', () => {
  const now = Date.parse('2020-01-01T00:00:00Z');

  it('converts timestamps to rounded seconds and clamps clocks in the future', () => {
    expect(secondsSince(new Date(now - 3_600).toISOString(), now)).toBe(4);
    expect(secondsSince(new Date(now + 5_000).toISOString(), now)).toBe(0);
    expect(secondsSince('invalid', now)).toBeNull();
    expect(secondsSince(null, now)).toBeNull();
  });

  it.each([
    [null, 'Not reported'],
    [0, '0s ago'],
    [59, '59s ago'],
    [60, '1m 0s ago'],
    [3_599, '59m 59s ago'],
    [3_600, '1h 0m ago'],
    [86_399, '23h 59m ago'],
    [86_400, '1d 0h ago']
  ])('formats age %s as %s', (seconds, expected) => {
    expect(formatAge(seconds)).toBe(expected);
  });

  it('marks reported and timestamp ages stale only after the threshold', () => {
    const atThreshold = new Date(
      now - CHECKPOINT_STALE_SECONDS * 1_000
    ).toISOString();
    const pastThreshold = new Date(
      now - (CHECKPOINT_STALE_SECONDS + 1) * 1_000
    ).toISOString();

    expect(ageStatus(atThreshold, now)).toMatchObject({
      stale: false,
      tone: ''
    });
    expect(ageStatus(pastThreshold, now)).toMatchObject({
      stale: true,
      tone: 'warning'
    });
    expect(reportedAgeStatus(CHECKPOINT_STALE_SECONDS)).toMatchObject({
      stale: false,
      tone: ''
    });
    expect(reportedAgeStatus(CHECKPOINT_STALE_SECONDS + 1)).toMatchObject({
      stale: true,
      tone: 'warning'
    });
    expect(ageStatus(null, now)).toEqual({
      seconds: null,
      label: 'Not reported',
      stale: false,
      tone: ''
    });
  });

  it('uses reported pending age, including zero, before falling back to its timestamp', () => {
    const old = new Date(
      now - (PENDING_RECOVERY_SECONDS + 1) * 1_000
    ).toISOString();

    expect(oldestPendingStatus(old, 0, now)).toMatchObject({
      seconds: 0,
      stale: false
    });
    expect(oldestPendingStatus(old, undefined, now)).toMatchObject({
      seconds: PENDING_RECOVERY_SECONDS + 1,
      stale: true
    });
    expect(
      oldestPendingStatus(null, PENDING_RECOVERY_SECONDS + 1, now)
    ).toMatchObject({ stale: true });
  });

  it('applies a caller-supplied threshold to timestamps and reported ages', () => {
    const at = new Date(now - 6_000).toISOString();

    expect(ageStatus(at, now, 5).stale).toBe(true);
    expect(ageStatus(at, now, 6).stale).toBe(false);
    expect(reportedAgeStatus(6, 5).stale).toBe(true);
  });
});
