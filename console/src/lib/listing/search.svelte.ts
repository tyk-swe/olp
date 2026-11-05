import { onDestroy } from 'svelte';

/// Pause between the last keystroke and applying a list's search filter.
export const SEARCH_DEBOUNCE_MS = 250;

/**
 * Debounces a search input's application. `schedule` waits for a pause in
 * typing; `applyNow` runs immediately (a cleared box or a companion filter
 * change); `cancel` drops a pending application without running it. The timer
 * is cancelled automatically when the owning component unmounts, so a queued
 * search never fires against a stale view.
 */
export function debouncedSearch(apply: () => void, ms = SEARCH_DEBOUNCE_MS) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const cancel = () => {
    if (timer === undefined) return;
    clearTimeout(timer);
    timer = undefined;
  };
  onDestroy(cancel);
  return {
    schedule() {
      cancel();
      timer = setTimeout(() => {
        timer = undefined;
        apply();
      }, ms);
    },
    applyNow() {
      cancel();
      apply();
    },
    cancel
  };
}
