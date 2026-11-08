<script lang="ts" generics="T">
  import type { Snippet } from 'svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { errorMessage } from '#lib/management/http.js';

  let {
    query,
    noun,
    isEmpty = () => false,
    empty,
    children
  }: {
    query: {
      readonly isPending: boolean;
      readonly error: unknown;
      readonly data: T | undefined;
      refetch: () => unknown;
    };
    noun: string;
    isEmpty?: (data: T) => boolean;
    empty?: Snippet;
    children: Snippet<[T]>;
  } = $props();
</script>

{#if query.data !== undefined}
  {#if empty && isEmpty(query.data)}
    {@render empty()}
  {:else}
    {@render children(query.data)}
  {/if}
{:else if query.error}
  <Callout tone="danger" title="Couldn't load {noun}">
    {errorMessage(query.error)}
    {#snippet actions()}
      <button
        class="button button-secondary"
        type="button"
        onclick={() => query.refetch()}>Retry</button
      >
    {/snippet}
  </Callout>
{:else}
  <p class="loading" role="status">Loading {noun}…</p>
{/if}

<style>
  .loading {
    padding: 2rem 0;
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    text-transform: uppercase;
    animation: fade var(--dur-enter) var(--ease-out) 200ms backwards;
  }
</style>
