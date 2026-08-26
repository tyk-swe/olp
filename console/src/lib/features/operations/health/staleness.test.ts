import { describe, expect, it } from 'vitest';
import {
  CHECKPOINT_STALE_SECONDS,
  MAINTENANCE_STALE_SECONDS,
  PENDING_RECOVERY_SECONDS,
  ageStatus,
  formatAge,
  oldestPendingStatus,
  reportedAgeStatus,
  secondsSince
} from './staleness';

const now = Date.parse('2026-07-12T12:00:00Z');

describe('secondsSince', () => {
  it('returns null for a missing or unparseable timestamp', () => {
    expect(secondsSince(null, now)).toBeNull();
    expect(secondsSince(undefined, now)).toBeNull();
    expect(secondsSince('not a timestamp', now)).toBeNull();
  });

  it('measures elapsed whole seconds', () => {
    expect(secondsSince('2026-07-12T11:59:41Z', now)).toBe(19);
  });

  it('clamps a checkpoint from a clock running ahead to zero', () => {
    expect(secondsSince('2026-07-12T12:00:05Z', now)).toBe(0);
  });
});

describe('formatAge', () => {
  it('reports missing ages instead of guessing zero', () => {
    expect(formatAge(null)).toBe('Not reported');
    expect(formatAge(undefined)).toBe('Not reported');
  });

  it('scales the unit with the age', () => {
    expect(formatAge(19)).toBe('19s ago');
    expect(formatAge(185)).toBe('3m 5s ago');
    expect(formatAge(3_900)).toBe('1h 5m ago');
    expect(formatAge(90_000)).toBe('1d 1h ago');
  });
});

describe('ageStatus', () => {
  it('stays clear at the 20 second checkpoint threshold and warns past it', () => {
    expect(ageStatus('2026-07-12T11:59:40Z', now).stale).toBe(false);
    expect(ageStatus('2026-07-12T11:59:40Z', now).tone).toBe('');
    const past = ageStatus('2026-07-12T11:59:39Z', now);
    expect(past.seconds).toBe(CHECKPOINT_STALE_SECONDS + 1);
    expect(past.stale).toBe(true);
    expect(past.tone).toBe('warning');
  });

  it('applies the 180 second maintenance threshold when asked', () => {
    const age = ageStatus('2026-07-12T11:58:00Z', now, MAINTENANCE_STALE_SECONDS);
    expect(age.seconds).toBe(120);
    expect(age.stale).toBe(false);
    expect(ageStatus('2026-07-12T11:56:00Z', now, MAINTENANCE_STALE_SECONDS).stale).toBe(true);
  });

  it('never calls an absent checkpoint stale', () => {
    expect(ageStatus(null, now)).toEqual({
      seconds: null,
      label: 'Not reported',
      stale: false,
      tone: ''
    });
  });
});

describe('reportedAgeStatus', () => {
  it('uses the age the backend already computed', () => {
    expect(reportedAgeStatus(21)).toEqual({
      seconds: 21,
      label: '21s ago',
      stale: true,
      tone: 'warning'
    });
    expect(reportedAgeStatus(null).label).toBe('Not reported');
  });
});

describe('oldestPendingStatus', () => {
  it('prefers the reported age over the timestamp', () => {
    const age = oldestPendingStatus('2026-07-12T11:00:00Z', 12, now);
    expect(age.seconds).toBe(12);
    expect(age.stale).toBe(false);
  });

  it('falls back to the timestamp when no age was reported', () => {
    expect(oldestPendingStatus('2026-07-12T11:59:00Z', null, now).seconds).toBe(60);
  });

  it('warns once recovery should have begun', () => {
    expect(oldestPendingStatus(null, PENDING_RECOVERY_SECONDS, now).stale).toBe(false);
    expect(oldestPendingStatus(null, PENDING_RECOVERY_SECONDS + 1, now).stale).toBe(true);
  });
});
