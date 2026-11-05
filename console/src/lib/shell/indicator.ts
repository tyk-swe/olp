import { tick } from 'svelte';

/**
 * Slides one shared indicator beneath the active item of a navigation row or
 * tab strip. The indicator is the node's own `::after`, positioned from the
 * custom properties set here, so the markup stays a plain list of links or
 * buttons. The first placement is instant (`data-indicator="placed"`); later
 * moves slide (`data-indicator="sliding"`). The attribute is removed while no
 * item in the row is `.active`, and the per-item underline remains the
 * fallback until then. Pass any value that changes with the active item as
 * the parameter; the row is re-measured once Svelte has applied the matching
 * `active` classes.
 */
export function slidingIndicator(node: HTMLElement, key?: unknown) {
  let placed = false;
  let mounted = true;

  function measure() {
    if (!mounted) return;
    const active = node.querySelector<HTMLElement>('.active');
    if (!active) {
      delete node.dataset.indicator;
      placed = false;
      return;
    }
    node.style.setProperty('--indicator-x', `${active.offsetLeft}px`);
    node.style.setProperty('--indicator-w', `${active.offsetWidth}`);
    node.dataset.indicator = placed ? 'sliding' : 'placed';
    placed = true;
  }

  measure();
  const observer =
    typeof ResizeObserver === 'undefined'
      ? undefined
      : new ResizeObserver(() => measure());
  observer?.observe(node);

  return {
    update(next: unknown) {
      if (next === key) return;
      key = next;
      void tick().then(measure);
    },
    destroy() {
      mounted = false;
      observer?.disconnect();
    }
  };
}
