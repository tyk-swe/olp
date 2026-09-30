// @vitest-environment jsdom
import { tick } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { slidingIndicator } from '$lib/components/indicator';

let row: HTMLUListElement;

function navigationRow(...links: [left: number, width: number][]) {
  row = document.createElement('ul');
  for (const [left, width] of links) {
    const link = document.createElement('a');
    Object.defineProperty(link, 'offsetLeft', { value: left });
    Object.defineProperty(link, 'offsetWidth', {
      value: width,
      configurable: true
    });
    row.append(link);
  }
  document.body.append(row);
  return [...row.querySelectorAll('a')];
}

afterEach(() => {
  row.remove();
  vi.unstubAllGlobals();
});

it('places the indicator under the active link without sliding on first render', () => {
  const [, second] = navigationRow([0, 80], [88, 64]);
  second.classList.add('active');
  const action = slidingIndicator(row, '/routes');
  expect(row.style.getPropertyValue('--indicator-x')).toBe('88px');
  expect(row.style.getPropertyValue('--indicator-w')).toBe('64');
  expect(row.dataset.indicator).toBe('placed');
  action.destroy();
});

it('slides to the next active link once the classes have been applied', async () => {
  const [first, second] = navigationRow([0, 80], [88, 64]);
  first.classList.add('active');
  const action = slidingIndicator(row, '/');
  first.classList.remove('active');
  second.classList.add('active');
  action.update('/routes');
  expect(row.style.getPropertyValue('--indicator-x')).toBe('0px');
  await tick();
  expect(row.style.getPropertyValue('--indicator-x')).toBe('88px');
  expect(row.dataset.indicator).toBe('sliding');
  action.destroy();
});

it('hides the indicator while no link in the row is active', async () => {
  const [first] = navigationRow([0, 80]);
  first.classList.add('active');
  const action = slidingIndicator(row, '/');
  first.classList.remove('active');
  action.update('/settings/profile');
  await tick();
  expect(row.dataset.indicator).toBeUndefined();
  action.destroy();
});

it('follows layout changes and stops observing when destroyed', () => {
  const observe = vi.fn();
  const disconnect = vi.fn();
  let resized = () => {};
  vi.stubGlobal(
    'ResizeObserver',
    class {
      constructor(callback: () => void) {
        resized = callback;
      }
      observe = observe;
      disconnect = disconnect;
    }
  );
  const [first] = navigationRow([0, 80]);
  first.classList.add('active');
  const action = slidingIndicator(row, '/');
  expect(observe).toHaveBeenCalledWith(row);
  Object.defineProperty(first, 'offsetWidth', { value: 96 });
  resized();
  expect(row.style.getPropertyValue('--indicator-w')).toBe('96');
  action.destroy();
  expect(disconnect).toHaveBeenCalled();
});
