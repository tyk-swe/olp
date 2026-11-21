import { mount, unmount } from 'svelte';
import { expect, it, onTestFinished } from 'vitest';
import ApiKeyOverlaps from './ApiKeyOverlaps.svelte';

it('distinguishes missing, observed and degraded evidence for each overlap', () => {
  const target = document.createElement('div');
  const expires = '2026-01-01T10:00:00Z';
  const observed = '2026-01-01T09:05:00Z';
  const component = mount(ApiKeyOverlaps, {
    target,
    props: {
      overlaps: [
        {
          lookup_id: 'healthy-empty',
          expires_at: expires,
          last_observed_use: null,
          observation_status: 'ok'
        },
        {
          lookup_id: 'healthy-observed',
          expires_at: expires,
          last_observed_use: observed,
          observation_status: 'ok'
        },
        {
          lookup_id: 'degraded-empty',
          expires_at: expires,
          last_observed_use: null,
          observation_status: 'degraded'
        },
        {
          lookup_id: 'degraded-observed',
          expires_at: expires,
          last_observed_use: observed,
          observation_status: 'degraded'
        }
      ]
    }
  });
  onTestFinished(() => unmount(component));
  const [empty, healthy, unavailable, stale] =
    target.querySelectorAll('tbody tr');
  expect(empty.textContent).toContain('No use observed');
  expect(healthy.querySelector('time')?.dateTime).toBe(observed);
  expect(unavailable.textContent).toContain(
    'Observation unavailable (recording failed)'
  );
  expect(unavailable.textContent).toContain(
    'The old secret may still be in use.'
  );
  expect(unavailable.textContent).not.toContain('No use observed');
  expect(stale.textContent).toContain('May be stale (recording failed)');
  expect(stale.textContent).not.toContain('No use observed');
  expect(stale.querySelector('time')?.dateTime).toBe(observed);
});
