<script lang="ts">
  import type { Snippet } from 'svelte';

  let { label, children }: { label: string; children: Snippet } = $props();
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
<div
  class="table-shell scroll-table"
  tabindex="0"
  role="region"
  aria-label={label}
>
  {@render children()}
</div>
