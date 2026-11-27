<script lang="ts">
  import type { Snippet } from 'svelte';
  import { errorMessage } from '#lib/management/http.js';
  import Callout from '#lib/components/Callout.svelte';

  let {
    query,
    loading,
    failure,
    errorTitle,
    retryLabel = 'Retry',
    updating = false,
    compact = false,
    emptyWhen,
    empty,
    children
  }: {
    query: {
      isPending: boolean;
      isError: boolean;
      error: unknown;
      refetch: () => Promise<unknown>;
    };
    loading: string;
    failure?: string;
    errorTitle?: string;
    retryLabel?: string;
    updating?: boolean;
    compact?: boolean;
    emptyWhen?: boolean;
    empty?: Snippet;
    children: Snippet;
  } = $props();
</script>

{#if query.isPending}
  <div class="loading-state" class:compact role="status">{loading}</div>
{:else if query.isError}
  <Callout tone="danger" title={errorTitle}>
    {failure ?? errorMessage(query.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => query.refetch()}
      >
        {retryLabel}
      </button>{/snippet}
  </Callout>
{:else if emptyWhen && empty}
  {@render empty()}
{:else}
  {#if updating}<p class="updating" role="status">Updating…</p>{/if}
  {@render children()}
{/if}
