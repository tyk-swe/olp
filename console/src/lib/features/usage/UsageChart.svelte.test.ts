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

it('keeps keyboard inspection through pointer leave until Escape or blur', () => {
  const { target } = mountWithState(
    UsageChart,
    { points, granularity: 'hour' },
    { attach: true }
  );

  expect(target.querySelector('svg')).not.toBeNull();

  const stage = target.querySelector<HTMLInputElement>(
    'input[type="range"][aria-label="Inspect usage bucket"]'
  );
  const chart = target.querySelector('.chart');
  expect(chart).not.toBeNull();
  stage?.focus();
  expect(stage).toBe(document.activeElement);

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

  chart?.dispatchEvent(new MouseEvent('pointerleave'));
  flushSync();
  expect(stage).toBe(document.activeElement);
  expect(target.querySelector('.readout')?.textContent).toContain('28 req');
  expect(stage?.getAttribute('aria-valuetext')).toContain('28 requests');

  stage?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })
  );
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain(
    'Hover or focus chart to inspect buckets'
  );

  stage?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })
  );
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain('12 req');

  stage?.blur();
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain(
    'Hover or focus chart to inspect buckets'
  );
  expect(stage?.getAttribute('aria-valuetext')).toBe('No bucket selected');
});

it('selects the bucket plotted under the pointer when buckets are unevenly spaced', () => {
  const { target } = mountWithState(UsageChart, {
    points: [
      makePoint({ bucket: '2026-10-01T00:00:00Z', request_count: 12 }),
      makePoint({ bucket: '2026-10-01T01:00:00Z', request_count: 28 }),
      makePoint({ bucket: '2026-10-01T10:00:00Z', request_count: 5 })
    ],
    granularity: 'hour'
  });
  const chart = target.querySelector<HTMLElement>('.chart');
  if (!chart) throw new Error('chart did not render');
  chart.getBoundingClientRect = () =>
    ({ left: 0, top: 0, width: 640, height: 288 }) as DOMRect;
  const secondX = Number(
    target.querySelectorAll('circle.point')[1]?.getAttribute('cx')
  );

  chart.dispatchEvent(
    new MouseEvent('pointerdown', { clientX: secondX, bubbles: true })
  );
  flushSync();

  expect(target.querySelector('.readout')?.textContent).toContain('28 req');

  chart.dispatchEvent(new MouseEvent('pointerleave'));
  flushSync();
  expect(target.querySelector('.readout')?.textContent).toContain(
    'Hover or focus chart to inspect buckets'
  );
});
