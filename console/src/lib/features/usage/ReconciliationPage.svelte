<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import Callout from '#lib/components/Callout.svelte';
  import ReconciliationPanel from './ReconciliationPanel.svelte';
  import {
    applyUsageDraft,
    defaultUsageState,
    readUsageState,
    usageDraft,
    usageProblem,
    usageSearch
  } from './usageState.js';

  const applied = $derived(
    readUsageState(page.url.searchParams, defaultUsageState())
  );
  const problem = $derived(usageProblem(applied));
  const shown = $derived(usageDraft(applied));

  function apply(event: SubmitEvent) {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const draft = {
      ...shown,
      start: String(form.get('start') ?? ''),
      end: String(form.get('end') ?? '')
    };
    void goto(
      resolve(
        `usage/reconciliation?${usageSearch(applyUsageDraft(draft, applied))}`
      )
    );
  }
</script>

<svelte:head><title>Reconciliation · OpenLLMProxy</title></svelte:head>

<form class="toolbar range" aria-label="Time range" onsubmit={apply}>
  <label class="filter-control"
    >From <input
      name="start"
      value={shown.start}
      type="datetime-local"
      required
    /></label
  >
  <label class="filter-control"
    >To <input
      name="end"
      value={shown.end}
      type="datetime-local"
      required
    /></label
  >
  <button class="button button-secondary" type="submit">Apply</button>
</form>

{#if problem}
  <Callout tone="danger">{problem}</Callout>
{:else}
  <ReconciliationPanel
    start={applied.filters.start}
    end={applied.filters.end}
  />
{/if}

<style>
  .range {
    align-items: end;
    justify-content: flex-start;
  }
</style>
