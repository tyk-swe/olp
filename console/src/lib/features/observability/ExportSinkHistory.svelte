<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { formatCost, formatDate } from '$lib/format';
  import { listExportSinkFocusDatasets, listExportSinkGaps } from './api';
  import { observabilityKeys } from './observabilityKeys';
  import { focusDatasetStatus } from './presentation';
  let { sinkId, kind }: { sinkId: string; kind: 'gaps' | 'datasets' } =
    $props();
  const gapQuery = createQuery(() => ({
    queryKey: observabilityKeys.gaps(sinkId),
    queryFn: ({ signal }) => listExportSinkGaps(sinkId, signal),
    enabled: kind === 'gaps'
  }));
  let datasetCursors = $state<string[]>([]);
  const datasetCursor = $derived(datasetCursors.at(-1));
  const focusDatasets = createQuery(() => ({
    queryKey: observabilityKeys.focusDatasets(sinkId, datasetCursor),
    queryFn: ({ signal }) =>
      listExportSinkFocusDatasets(sinkId, datasetCursor, signal),
    enabled: kind === 'datasets'
  }));
</script>

{#if kind === 'gaps'}
  {#if gapQuery.isPending}<span class="inline-status">Loading gaps…</span
    >{:else if gapQuery.isError}<span class="inline-problem"
      >Gap records are unavailable.</span
    >{:else if !(gapQuery.data ?? []).length}<span class="section-help"
      >No export gaps recorded.</span
    >{:else}<ul class="gap-list">
      {#each gapQuery.data ?? [] as gap (gap.id)}<li>
          <span class="mono">{gap.stream}</span> — {gap.record_count}
          records, {formatDate(gap.first_occurred_at)} → {formatDate(
            gap.last_occurred_at
          )}: {gap.reason}
        </li>{/each}
    </ul>{/if}
{:else}
  {#if focusDatasets.isPending}<span class="inline-status" role="status"
      >Loading datasets…</span
    >{:else if focusDatasets.isError}<span class="inline-problem" role="alert"
      >Focus datasets are unavailable.</span
    >{:else if !(focusDatasets.data?.items ?? []).length}<span
      class="section-help">No datasets delivered yet.</span
    >{:else}<table class="data-table focus-datasets">
      <thead
        ><tr
          ><th scope="col">Charge date</th><th scope="col">Revision</th><th
            scope="col">Delivered</th
          ><th scope="col">Rows</th><th scope="col">Billed cost</th><th
            scope="col">Commitments</th
          ><th scope="col">Status</th></tr
        ></thead
      ><tbody>
        {#each focusDatasets.data?.items ?? [] as dataset (dataset.charge_date)}
          <tr
            ><td class="mono">{dataset.charge_date}</td><td
              >{dataset.revision}</td
            ><td
              >{dataset.delivered_at
                ? formatDate(dataset.delivered_at)
                : 'pending'}</td
            ><td>{dataset.row_count}</td><td
              >{formatCost(dataset.billed_cost, null)}</td
            ><td
              >{#if dataset.commitment_totals}
                <div>
                  Purchased: {dataset.commitment_totals.purchased_cost}
                </div>
                <div>
                  Allocated: {dataset.commitment_totals.allocated_cost}
                </div>
                <div>
                  Unused: {dataset.commitment_totals.unused_cost}
                </div>
                <div>
                  {dataset.commitment_totals.provisional
                    ? 'Provisional'
                    : 'Closed'} · {dataset.commitment_totals.complete
                    ? 'Complete'
                    : 'Incomplete coverage'}
                </div>
              {:else}Unavailable{/if}</td
            ><td>{focusDatasetStatus(dataset)}</td></tr
          >
        {/each}
      </tbody>
    </table>{/if}
  <div class="form-actions" role="group" aria-label="Dataset pages">
    {#if datasetCursors.length > 0}
      <button
        class="button button-secondary"
        type="button"
        disabled={focusDatasets.isFetching}
        onclick={() => {
          datasetCursors = datasetCursors.slice(0, -1);
        }}>Newer datasets</button
      >
    {/if}
    {#if focusDatasets.isError}
      <button
        class="button button-secondary"
        type="button"
        disabled={focusDatasets.isFetching}
        onclick={() => focusDatasets.refetch()}>Retry datasets</button
      >
    {/if}
    {#if focusDatasets.data?.nextCursor}
      <button
        class="button button-secondary"
        type="button"
        disabled={focusDatasets.isFetching}
        onclick={() => {
          const cursor = focusDatasets.data?.nextCursor;
          if (cursor) datasetCursors = [...datasetCursors, cursor];
        }}>Older datasets</button
      >
    {/if}
  </div>
{/if}
