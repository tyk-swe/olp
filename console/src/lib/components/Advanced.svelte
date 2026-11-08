<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    label = 'Advanced',
    open = false,
    children
  }: { label?: string; open?: boolean; children: Snippet } = $props();
</script>

<details class="advanced" {open}>
  <summary>{label}</summary>
  <div class="content">{@render children()}</div>
</details>

<style>
  .advanced {
    border-top: 1px solid var(--border-hairline);
  }

  summary {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.5rem;
    color: var(--foreground-muted);
    cursor: pointer;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    letter-spacing: -0.24px;
    list-style: none;
    text-transform: uppercase;
    transition: color var(--motion);
  }

  summary::-webkit-details-marker {
    display: none;
  }

  summary::before {
    content: '';
    width: 0.4rem;
    height: 0.4rem;
    border-right: 1px solid currentColor;
    border-bottom: 1px solid currentColor;
    transform: rotate(-45deg);
    transition: transform var(--motion);
  }

  .advanced[open] > summary::before {
    transform: rotate(45deg);
  }

  summary:hover {
    color: var(--foreground);
  }

  .content {
    display: grid;
    gap: 1rem;
    padding-bottom: 1rem;
  }
</style>
