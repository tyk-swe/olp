<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatDate } from '#lib/format.js';
  import ExportSinkHistory from './ExportSinkHistory.svelte';
  import MeteringDeliveryLookup from './MeteringDeliveryLookup.svelte';
  import type { ExportSink } from './api';
  import {
    lagSeconds,
    sinkSettingsSummary,
    streamState,
    streamStatuses
  } from './presentation';

  let {
    canCreate,
    onCreate,
    pending,
    failed,
    items,
    onRetry,
    canMutate,
    busy,
    gapSinkId = $bindable(),
    datasetSinkId = $bindable(),
    onEdit,
    onToggle,
    onGaps,
    onDatasets,
    onRemove
  }: {
    canCreate: boolean;
    onCreate: () => void;
    pending: boolean;
    failed: boolean;
    items: ExportSink[];
    onRetry: () => void;
    canMutate: (projectId: string | null | undefined) => boolean;
    busy: boolean;
    gapSinkId: string;
    datasetSinkId: string;
    onEdit: (sink: ExportSink) => void;
    onToggle: (sink: ExportSink) => void;
    onGaps: (sink: ExportSink) => void;
    onDatasets: (sink: ExportSink) => void;
    onRemove: (sink: ExportSink) => void;
  } = $props();
  let deliverySinkId = $state('');
  function toggleDelivery(sink: ExportSink) {
    deliverySinkId =
      deliverySinkId === sink.export_sink_id ? '' : sink.export_sink_id;
  }
</script>

{#if canCreate}<div class="toolbar">
    <p class="section-help">
      Send logs, traces and metrics to an OpenTelemetry collector or another
      endpoint.
    </p>
    <div class="page-actions">
      <button class="button button-primary" type="button" onclick={onCreate}>
        New sink
      </button>
    </div>
  </div>{/if}
{#if pending}<span class="inline-status" role="status">Loading sinks…</span>
{:else if failed}<span class="inline-problem" role="alert">
    Sinks are unavailable.
    <button class="text-button" type="button" onclick={onRetry}>Retry</button>
  </span>
{:else if !items.length}<EmptyState title="No telemetry sinks yet">
    Add a sink to send logs and traces to your own collector.
  </EmptyState>
{:else}
  <ScrollTable label="Export sinks">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Name</th><th scope="col">Type</th><th scope="col">
            Project
          </th><th scope="col">Streams</th><th scope="col">Delivery</th><th
            scope="col"
          >
            Enabled
          </th><th scope="col"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {#each items as sink (sink.export_sink_id)}
          <tr>
            <td>
              <strong>{sink.name}</strong><br /><small class="mono">
                {sink.destination}
              </small>{#if sinkSettingsSummary(sink)}<br /><small>
                  {sinkSettingsSummary(sink)}
                </small>{/if}
            </td><td>{sink.type} · {sink.format}</td><td>
              {sink.project_id ? 'Project' : 'Installation-wide'}
            </td><td>{sink.streams.join(', ')}</td><td>
              {#each streamStatuses(sink) as st (st.stream)}
                {@const errorCodes = st.pending_error_codes ?? []}
                <div class="stream-status">
                  <span class="mono">{st.stream}</span> — {streamState(st)}:
                  {st.pending ?? 0} pending, {st.delivered_total ?? 0} delivered,
                  {st.failures_total ?? 0} failed, {st.gaps_total ?? 0} gaps{#if errorCodes.length}<br
                    /><small>
                      errors: {errorCodes.join(', ')}
                    </small>{/if}{#if (st.pending ?? 0) > 0}<br /><small>
                      {st.oldest_pending_at &&
                      Number.isFinite(Date.parse(st.oldest_pending_at))
                        ? `approx. lag ${Math.round(lagSeconds(st))}s`
                        : 'lag unknown'}
                    </small>{/if}{#if st.last_success_at}<br /><small>
                      last success {formatDate(st.last_success_at)}
                    </small>{/if}{#if st.last_attempt_at}<br /><small>
                      last attempt {formatDate(st.last_attempt_at)}
                    </small>{/if}
                </div>
              {/each}
            </td><td>{sink.enabled ? 'Yes' : 'No'}</td><td>
              <button
                class="text-button"
                type="button"
                onclick={() => onGaps(sink)}
              >
                Gaps
              </button>{#if sink.format === 'cloudevents'}<button
                  class="text-button"
                  type="button"
                  onclick={() => toggleDelivery(sink)}
                >
                  Delivery lookup
                </button>{/if}{#if sink.format === 'csv_gzip'}<button
                  class="text-button"
                  type="button"
                  onclick={() => onDatasets(sink)}
                >
                  Datasets
                </button>{/if}{#if canMutate(sink.project_id)}<button
                  class="text-button"
                  type="button"
                  disabled={busy}
                  onclick={() => onEdit(sink)}
                >
                  Edit
                </button><button
                  class="text-button"
                  type="button"
                  disabled={busy}
                  onclick={() => onToggle(sink)}
                >
                  {sink.enabled ? 'Disable' : 'Enable'}
                </button><button
                  class="text-button"
                  type="button"
                  disabled={busy}
                  onclick={() => onRemove(sink)}
                >
                  Delete
                </button>{/if}
            </td>
          </tr>
          {#if gapSinkId === sink.export_sink_id}
            <tr class="gap-row">
              <td colspan="7">
                <ExportSinkHistory sinkId={sink.export_sink_id} kind="gaps" />
              </td>
            </tr>
          {/if}
          {#if deliverySinkId === sink.export_sink_id}
            <tr class="gap-row">
              <td colspan="7">
                <MeteringDeliveryLookup sinkId={sink.export_sink_id} />
              </td>
            </tr>
          {/if}
          {#if datasetSinkId === sink.export_sink_id}
            <tr class="gap-row">
              <td colspan="7">
                <ExportSinkHistory
                  sinkId={sink.export_sink_id}
                  kind="datasets"
                />
              </td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
