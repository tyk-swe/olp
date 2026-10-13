import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import RoutingPreferencesForm from './RoutingPreferencesForm.svelte';

let host: HTMLElement;
let component: ReturnType<typeof mount>;
const onChange = vi.fn();

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  component = mount(RoutingPreferencesForm, {
    target: host,
    props: { value: '{"strategy":"latency"}', onChange }
  });
  flushSync();
});

afterEach(async () => {
  await unmount(component);
  host.remove();
});

describe.each(['preferred_max_latency_ms', 'preferred_min_throughput'])(
  '%s',
  (key) => {
    function input() {
      return host.querySelector<HTMLInputElement>(
        `#routing-preferences-${key}`
      )!;
    }

    function enter(value: string) {
      input().value = value;
      input().dispatchEvent(new Event('input', { bubbles: true }));
      flushSync();
      return JSON.parse(onChange.mock.lastCall![0]);
    }

    it.each(['0', '-1', '0.5', '1.5'])(
      'rejects %s in the control and serialized preferences',
      (value) => {
        enter('10');
        input().value = value;
        expect(input().checkValidity()).toBe(false);
        expect(enter(value)).toEqual({ strategy: 'latency' });
      }
    );

    it.each(['1', '250'])('accepts the positive integer %s', (value) => {
      expect(enter(value)).toEqual({
        strategy: 'latency',
        [key]: Number(value)
      });
      expect(input().checkValidity()).toBe(true);
    });

    it('clears the preference when the field is emptied', () => {
      enter('10');
      expect(enter('')).toEqual({ strategy: 'latency' });
      expect(input().checkValidity()).toBe(true);
    });
  }
);

describe('max_cost', () => {
  function input() {
    return host.querySelector<HTMLInputElement>(
      '#routing-preferences-max-cost'
    )!;
  }

  it('writes the decimal ceiling into preferences', () => {
    input().value = '0.5';
    input().dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    expect(JSON.parse(onChange.mock.lastCall![0])).toEqual({
      strategy: 'latency',
      max_cost: '0.5'
    });
  });

  it('clears it with an empty value', () => {
    input().value = '0.5';
    input().dispatchEvent(new Event('input', { bubbles: true }));
    input().value = '';
    input().dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    expect(JSON.parse(onChange.mock.lastCall![0])).toEqual({
      strategy: 'latency'
    });
  });

  it('shows the stored ceiling', () => {
    unmount(component);
    component = mount(RoutingPreferencesForm, {
      target: host,
      props: {
        value: '{"max_cost":"0.001"}',
        constraintsOnly: true,
        onChange
      }
    });
    flushSync();
    expect(input().value).toBe('0.001');
  });
});
