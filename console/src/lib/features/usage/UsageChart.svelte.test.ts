import { expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { mountWithState } from '#test/mount.js';
import type { UsagePoint } from '#lib/features/usage/usage.js';
import UsageChart from './UsageChart.svelte';

function makePoint(overrides: Partial<UsagePoint>): UsagePoint {
  return {
    bucket: '2026-10-01T00:00:00Z',
    request_count: 0,
    input_tokens: '0',
    cached_input_tokens: '0',
    cache_write_input_tokens: '0',
    cache_write_5m_input_tokens: '0',
    cache_write_1h_input_tokens: '0',
    output_tokens: '0',
    estimated_cost: '0',
    currency: 'USD',
    incomplete_count: 0,
    unpriced_count: 0,
    estimated_attempt_count: 0,
    estimated_input_tokens: '0',
    reported_input_tokens: '0',
    media_units: '0',
    audio_input_tokens: '0',
    cached_audio_input_tokens: '0',
    audio_output_tokens: '0',
    image_input_tokens: '0',
    cached_image_input_tokens: '0',
    image_output_tokens: '0',
    allocated_commitment: '0',
    charge: null,
    uncharged_attempt_count: 0,
    ...overrides
  };
}

const points: UsagePoint[] = [
  makePoint({
    bucket: '2026-10-01T00:00:00Z',
    request_count: 12,
    input_tokens: '1000',
    output_tokens: '200',
    estimated_cost: '0.015'
  }),
  makePoint({
    bucket: '2026-10-01T01:00:00Z',
    request_count: 28,
    input_tokens: '2400',
    output_tokens: '500',
    estimated_cost: '0.042'
  })
];

it('renders an empty state when there are no usage points', () => {
  const { target } = mountWithState(UsageChart, { points: [] });
  expect(target.textContent).toContain(
    'No usage was recorded in this time range.'
  );
  expect(target.querySelector('svg')).toBeNull();
});

it('renders without SVG distortion and updates readout via keyboard navigation', () => {
  const { target } = mountWithState(UsageChart, {
    points,
    granularity: 'hour'
  });

  const svg = target.querySelector('svg');
  expect(svg).not.toBeNull();
  expect(svg?.getAttribute('preserveAspectRatio')).toBeNull();

  const stage = target.querySelector<HTMLInputElement>(
    'input[type="range"][aria-label="Inspect usage bucket"]'
  );
  expect(stage).not.toBeNull();

  stage?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })
  );
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain('12 req');

  stage?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })
  );
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain('28 req');

  stage?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })
  );
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain(
    'Hover or focus chart to inspect buckets'
  );
});
