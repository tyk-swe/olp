<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    tone = 'info',
    title,
    children,
    actions
  }: {
    tone?: 'info' | 'warning' | 'danger' | 'success';
    title?: string;
    children: Snippet;
    actions?: Snippet;
  } = $props();
</script>

<div
  class="callout {tone}"
  role={tone === 'danger' ? 'alert' : tone === 'success' ? 'status' : 'note'}
>
  <div class="body">
    {#if title}<strong>{title}</strong>{/if}
    <div class="text">{@render children()}</div>
  </div>
  {#if actions}<div class="actions">{@render actions()}</div>{/if}
</div>

<style>
  .callout {
    --tone: var(--signal);
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem 1rem;
    padding: 0.75rem 1rem;
    border: 1px solid var(--border-hairline);
    border-left: 2px solid var(--tone);
    border-radius: var(--radius-control);
    background: color-mix(in srgb, var(--tone) 6%, var(--surface));
    color: var(--foreground-subtle);
    font-size: var(--text-body-sm);
    line-height: 1.5;
  }

  .warning {
    --tone: var(--warning);
  }

  .danger {
    --tone: var(--danger);
  }

  .success {
    --tone: var(--success);
  }

  .body {
    display: grid;
    min-width: 0;
    flex: 1 1 18rem;
    gap: 0.15rem;
  }

  strong {
    color: var(--foreground);
  }

  .text :global(p) {
    margin: 0;
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }

  @media (forced-colors: active) {
    .callout {
      border-left-width: 4px;
    }
  }
</style>
