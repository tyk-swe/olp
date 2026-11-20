<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    label,
    busy,
    children
  }: { label: string; busy?: boolean; children: Snippet } = $props();
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
<div
  class="table-shell scroll-table"
  tabindex="0"
  role="region"
  aria-label={label}
  aria-busy={busy}
>
  {@render children()}
</div>
