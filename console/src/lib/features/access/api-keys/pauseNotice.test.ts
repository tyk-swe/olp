import { afterEach, describe, expect, it, vi } from 'vitest';
import { pauseNotice } from './pauseNotice';
import type { ApiKeyPauseRollout } from './api';

const saved = 'Pause saved. Enforcement across replicas is not yet confirmed.';
const enforced = 'Pause enforced across serving replicas';

function rollout(
  replicas: (number | null)[],
  overrides: Partial<ApiKeyPauseRollout> = {}
): ApiKeyPauseRollout {
  return {
    paused: true,
    paused_at: '2026-01-01T00:00:00Z',
    paused_authority_sequence: 42,
    enforced: false,
    replicas: replicas.map((sequence, index) => ({
      gateway_instance: `gateway-${index}`,
      authority_sequence: sequence
    })),
    ...overrides
  };
}

afterEach(() => {
  vi.useRealTimers();
});

describe('api key pause confirmation wording', () => {
  it('keeps the unconfirmed wording while one replica still serves older authority', () => {
    expect(pauseNotice(true, rollout([42, 41], { enforced: false }))).toBe(
      saved
    );
  });

  it('does not change on a timer or a missing replica report', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const unreported = rollout([42, null], { enforced: false });
    expect(pauseNotice(true, unreported)).toBe(saved);
    vi.advanceTimersByTime(10 * 60 * 1000);
    expect(pauseNotice(true, unreported)).toBe(saved);
    expect(pauseNotice(true, null)).toBe(saved);
    expect(pauseNotice(true, undefined)).toBe(saved);
  });

  it('does not treat a write generation or updated row as fleet confirmation', () => {
    expect(pauseNotice(true, rollout([], { enforced: true }))).toBe(saved);
    expect(
      pauseNotice(
        true,
        rollout([42], { enforced: true, paused_authority_sequence: null })
      )
    ).toBe(saved);
  });

  it('shows the enforced wording only for authoritative all-replicas evidence', () => {
    expect(pauseNotice(true, rollout([42, 43], { enforced: true }))).toBe(
      enforced
    );
    expect(
      pauseNotice(true, rollout([42], { enforced: true, paused: false }))
    ).toBe(saved);
    expect(pauseNotice(true, rollout([42], { enforced: false }))).toBe(saved);
  });

  it('shows neither confirmation after a failed save', () => {
    expect(pauseNotice(false, null)).toBeNull();
    expect(pauseNotice(false, rollout([42], { enforced: true }))).toBeNull();
  });
});
