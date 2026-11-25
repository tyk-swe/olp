<script lang="ts">
  import SavedFilters from './SavedFilters.svelte';
  import { usageKeys } from '#lib/features/usage/usageKeys.js';

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    usageBreakdown,
    usageSeries,
    usageSummary
  } from '#lib/features/usage/usage.js';
  import { getApiKey } from '#lib/features/access/index.js';
  import UsageDetails from './UsageDetails.svelte';
  import UsageReport from './UsageReport.svelte';
  import UsageFilterForm from './UsageFilterForm.svelte';
  import { completenessFromSummary } from '#lib/features/usage/completenessPresentation.js';
  import UsageCompletenessStatus from '#lib/features/usage/UsageCompletenessStatus.svelte';
  import { errorMessage } from '#lib/management/http.js';
  import { downloadBlob } from '#lib/download.js';
  import { readResult } from '#lib/editing/result.js';
  import { exportUsageCsv } from '#lib/features/usage/usage.js';

  import {
    applyUsageDraft,
    clearedUsageState,
    defaultUsageState,
    readUsageState,
    usageDraft,
    usageProblem,
    usageSearch,
    type UsageState
  } from '#lib/features/usage/usageState.js';

  // The default window is taken whenever the URL changes, so returning to a
  // bare /usage later shows the last 24 hours from then, not from mount.
  const applied = $derived(
    readUsageState(page.url.searchParams, defaultUsageState())
  );
  const urlProblem = $derived(usageProblem(applied));
  const hasNarrowingFilters = $derived(
    Object.entries(applied.filters).some(
      ([key, value]) => key !== 'start' && key !== 'end' && Boolean(value)
    )
  );
  let draft = $state(usageDraft(defaultUsageState()));
  let validation = $state<string | null>(null);

  $effect(() => {
    draft = usageDraft(applied);
    validation = null;
  });
  $effect(() => {
    if (!urlProblem && page.url.search !== `?${usageSearch(applied)}`) {
      void showUsage(applied, true);
    }
  });

  const queryClient = useQueryClient();
  const usage = createQuery(() => {
    const snapshot = applied;
    return {
      queryKey: usageKeys.report(JSON.stringify(snapshot)),
      enabled: !urlProblem,
      queryFn: async ({ signal }) => {
        const [summary, series, breakdown, apiKey] = await Promise.all([
          usageSummary(snapshot.filters),
          usageSeries(snapshot.filters, snapshot.granularity),
          usageBreakdown(snapshot.filters, snapshot.dimension),
          // The key only adds its budget; a key that can't be read, such
          // as a removed one, leaves the report itself intact.
          snapshot.filters.api_key_id
            ? getApiKey(snapshot.filters.api_key_id, signal).catch(() => null)
            : Promise.resolve(null)
        ]);
        return {
          summary,
          points: series.items,
          breakdown: breakdown.items,
          completeness: completenessFromSummary(summary),
          apiKey
        };
      }
    };
  });

  function refreshUsage() {
    void usage.refetch();
    void queryClient.invalidateQueries({
      queryKey: usageKeys.unpriced(applied.filters)
    });
  }

  function showUsage(state: UsageState, replace = false) {
    return goto(resolve(`usage?${usageSearch(state)}`), {
      replace,
      reset: false
    });
  }

  function apply(event: SubmitEvent) {
    event.preventDefault();
    const state = applyUsageDraft(draft, applied);
    validation = usageProblem(state);
    if (!validation) void showUsage(state);
  }

  function clear() {
    void showUsage(clearedUsageState(draft));
  }

  let exporting = $state(false);
  let exportError = $state('');

  async function exportCsv() {
    if (exporting) return;
    exporting = true;
    exportError = '';
    const result = await readResult(() =>
      exportUsageCsv(applied.filters, applied.dimension)
    );
    exporting = false;
    if (!result.ok) {
      exportError = errorMessage(
        result.error,
        'The export could not be created.'
      );
      return;
    }
    downloadBlob(
      new Blob([result.value], { type: 'text/csv' }),
      'olp-usage.csv'
    );
  }
</script>

<div class="toolbar">
  <SavedFilters
    scope="usage"
    search={`?${usageSearch(applied)}`}
    apply={(search) => {
      void showUsage(
        readUsageState(new URLSearchParams(search), defaultUsageState())
      );
    }}
  />
  <div class="page-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={refreshUsage}
      disabled={usage.isFetching || Boolean(urlProblem)}
    >
      Refresh
    </button><button
      class="button button-secondary"
      type="button"
      onclick={exportCsv}
      disabled={exporting || Boolean(urlProblem)}
    >
      {exporting ? 'Exporting…' : 'Download CSV'}
    </button>
  </div>
</div>
{#if exportError}<div class="inline-problem" role="alert">
    {exportError}
  </div>{/if}
<UsageFilterForm
  bind:draft
  {hasNarrowingFilters}
  onApply={apply}
  onReset={clear}
/>

{#if validation}<div class="inline-problem" role="alert">{validation}</div>{/if}

{#if urlProblem}
  <div class="inline-problem" role="alert">{urlProblem}</div>
{:else if usage.isPending}
  <div class="loading-state" role="status">Calculating usage…</div>
{:else if usage.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(usage.error, 'Usage could not be loaded.')}
    <button class="text-button" onclick={() => usage.refetch()}>
      Try again
    </button>
  </div>
{:else if usage.data}
  <UsageCompletenessStatus completeness={usage.data.completeness} />
  <UsageDetails
    apiKey={usage.data.apiKey}
    completeness={usage.data.completeness}
    summary={usage.data.summary}
  />

  <UsageReport
    report={{
      summary: usage.data.summary,
      points: usage.data.points,
      breakdown: usage.data.breakdown
    }}
    {applied}
  />
{/if}
