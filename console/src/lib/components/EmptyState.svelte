<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    title,
    children,
    action
  }: { title: string; children?: Snippet; action?: Snippet } = $props();
</script>

<div class="empty card">
  <h2>{title}</h2>
  {#if children}<p>{@render children()}</p>{/if}
  {#if action}<div class="action">{@render action()}</div>{/if}
</div>

<style>
  .empty {
    display: grid;
    justify-items: center;
    gap: 0.5rem;
    padding: clamp(2rem, 6vw, 3.5rem) 1.5rem;
    border-style: dashed;
    text-align: center;
  }

  h2 {
    font-size: 1.125rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }

  p {
    max-width: 34rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }

  .action {
    margin-top: 0.75rem;
  }
</style>
