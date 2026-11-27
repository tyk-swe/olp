<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    label,
    busy,
    children
  }: { label: string; busy?: boolean; children: Snippet } = $props();

  let shell = $state<HTMLDivElement>();
  let canScrollLeft = $state(false);
  let canScrollRight = $state(false);

  function measure() {
    if (!shell) return;
    canScrollLeft = shell.scrollLeft > 1;
    canScrollRight =
      shell.scrollLeft + shell.clientWidth < shell.scrollWidth - 1;
  }

  $effect(() => {
    const node = shell;
    if (!node) return;
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    if (node.firstElementChild) observer.observe(node.firstElementChild);
    return () => observer.disconnect();
  });
</script>

<div
  class="scroll-table"
  data-scroll-left={canScrollLeft || undefined}
  data-scroll-right={canScrollRight || undefined}
>
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
  <div
    bind:this={shell}
    class="table-shell"
    tabindex="0"
    role="region"
    aria-label={label}
    aria-busy={busy}
    onscroll={measure}
  >
    {@render children()}
  </div>
</div>

<style>
  .scroll-table {
    position: relative;
  }

  .scroll-table::before,
  .scroll-table::after {
    content: '';
    position: absolute;
    z-index: 2;
    top: 1px;
    bottom: 1px;
    width: 1.75rem;
    opacity: 0;
    pointer-events: none;
    transition: opacity var(--motion);
  }

  .scroll-table::before {
    left: 1px;
    border-top-left-radius: var(--radius-card);
    border-bottom-left-radius: var(--radius-card);
    background: linear-gradient(
      to right,
      color-mix(in srgb, var(--canvas) 75%, transparent),
      transparent
    );
  }

  .scroll-table::after {
    right: 1px;
    border-top-right-radius: var(--radius-card);
    border-bottom-right-radius: var(--radius-card);
    background: linear-gradient(
      to left,
      color-mix(in srgb, var(--canvas) 75%, transparent),
      transparent
    );
  }

  .scroll-table[data-scroll-left]::before,
  .scroll-table[data-scroll-right]::after {
    opacity: 1;
  }

  @media (forced-colors: active) {
    .scroll-table::before,
    .scroll-table::after {
      display: none;
    }
  }
</style>
