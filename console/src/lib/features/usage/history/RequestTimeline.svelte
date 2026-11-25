<script lang="ts">
  import { errorMessage } from '#lib/management/http.js';

  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RequestDetail } from '#lib/features/usage/history/api.js';
  import RequestFacts from './RequestFacts.svelte';
  import AttemptTimeline from './AttemptTimeline.svelte';
  let { detail }: { detail: CreateQueryResult<RequestDetail> } = $props();

  const cacheReasonLabels = new Map([
    ['no_store', 'Caller opted out (no-store)'],
    ['refresh', 'Caller requested a refresh (no-cache)'],
    ['multiple_choices', 'More than one response choice'],
    ['backend_unavailable', 'Cache backend unavailable']
  ]);
</script>

{#if detail.isPending}
  <div class="loading-state" role="status">Loading request timeline…</div>
{:else if detail.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(detail.error, 'The request timeline could not be loaded.')}
    <button class="text-button" onclick={() => detail.refetch()}>
      Try again
    </button>
  </div>
{:else if detail.data}
  {#if detail.data.cache_result}
    {@const cacheReason =
      cacheReasonLabels.get(detail.data.cache_lookup_reason ?? '') ??
      (detail.data.cache_result === 'bypass' ? 'Reason not recorded' : null)}
    <p class="muted">
      Response cache: {detail.data.cache_result}
      {#if cacheReason}
        · {cacheReason}{/if}
    </p>
  {/if}
  {#if detail.data.budget_boundary}
    <p class="inline-problem" role="status">
      Cost budget exhausted: {detail.data.budget_boundary.replaceAll('_', ' ')}.
    </p>
  {/if}
  <RequestFacts data={detail.data} />
  <AttemptTimeline data={detail.data} />
{/if}

<style>
  .text-button {
    padding: 0.4rem 0.65rem;
  }
</style>
