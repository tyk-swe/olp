import { tick } from 'svelte';

/** Call after validation so the rendered field error is available to assistive technology. */
export async function focusFormError(root: HTMLElement) {
  await tick();
  if (!root.isConnected) return;
  const invalid = root.querySelector<HTMLElement>(
    '[aria-invalid="true"]:not(:disabled)'
  );
  const summary = root.querySelector<HTMLElement>('[data-error-summary]');
  (invalid ?? summary)?.focus();
}

/** An error summary is also the fallback when the server cannot identify a field. */
export function focusErrorSummary(node: HTMLElement) {
  let mounted = true;
  void tick().then(() => {
    if (mounted && node.isConnected) node.focus();
  });
  return {
    destroy() {
      mounted = false;
    }
  };
}
