import { afterEach, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { mountWithState } from '#test/mount.js';
import Toaster from './Toaster.svelte';
import { notify, toasts } from './toasts.svelte.js';

afterEach(() => {
  toasts.clear();
  vi.useRealTimers();
});

it('renders toast tones and pauses auto-dismiss on pointer hover', () => {
  vi.useFakeTimers();
  const { target } = mountWithState(Toaster);

  notify('Rate limit near', 'warning');
  flushSync();

  const toast = target.querySelector<HTMLElement>('.toast');
  expect(toast?.classList.contains('warning')).toBe(true);
  expect(target.textContent).toContain('Rate limit near');

  vi.advanceTimersByTime(3000);
  toast?.dispatchEvent(new Event('pointerenter', { bubbles: true }));
  vi.advanceTimersByTime(5000);
  flushSync();
  expect(toasts.items).toHaveLength(1);

  toast?.dispatchEvent(new Event('pointerleave', { bubbles: true }));
  vi.advanceTimersByTime(2100);
  flushSync();
  expect(toasts.items).toHaveLength(0);
});

it('dismisses a toast when its close button is clicked', () => {
  const { target } = mountWithState(Toaster);

  notify('Saved', 'info');
  flushSync();
  expect(toasts.items).toHaveLength(1);

  target
    .querySelector<HTMLButtonElement>('button[aria-label="Dismiss"]')
    ?.click();
  flushSync();
  expect(toasts.items).toHaveLength(0);
});
