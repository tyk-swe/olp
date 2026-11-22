<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';

  import RequestFilters from './RequestFilters.svelte';
  import { errorMessage } from '#lib/management/http.js';
  import RequestResultsList from './RequestResultsList.svelte';
  import type { RequestListState } from '#lib/features/usage/history/requestListState.js';

  import { downloadBlob } from '#lib/download.js';
  import { readResult } from '#lib/editing/result.js';
  import { exportRequestsCsv } from '#lib/features/usage/history/api.js';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RequestSummary } from '#lib/features/usage/history/api.js';
  import type { CursorPage } from '#lib/management/http.js';
  let {
    requests,
    listState = $bindable(),
    applyFilters,
    resetFilters,
    problem,
    queryProblem
  }: {
    requests: CreateQueryResult<CursorPage<RequestSummary>>;
    listState: RequestListState;
    applyFilters: (event: SubmitEvent) => void;
    resetFilters: () => void;
    problem: string | null;
    queryProblem: string | null;
  } = $props();

  let exporting = $state(false);
  let exportError = $state('');

  async function exportCsv() {
    if (exporting) return;
    exporting = true;
    exportError = '';
    const result = await readResult(() => exportRequestsCsv(listState.applied));
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
      'olp-requests.csv'
    );
  }
</script>

<RequestFilters bind:listState onApply={applyFilters} onReset={resetFilters} />

{#if problem}<Callout tone="danger">{problem}</Callout>{/if}
<RequestResultsList
  {requests}
  {listState}
  {queryProblem}
  {exporting}
  {exportError}
  onExport={exportCsv}
/>
