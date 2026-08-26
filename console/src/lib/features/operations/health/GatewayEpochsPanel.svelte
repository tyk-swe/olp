<script lang="ts">
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    acknowledgeRequestMetadataGatewayEpoch,
    type Readiness,
    type RequestMetadataGatewayEpoch
  } from '$lib/api/health';
  import { errorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    type CursorHistory,
    type CursorPage
  } from '$lib/api/pagination';
  import { formatDate } from '$lib/format';

  let {
    epochs,
    pagination,
    readiness
  }: {
    epochs: CreateQueryResult<CursorPage<RequestMetadataGatewayEpoch>>;
    pagination: CursorHistory;
    readiness: CreateQueryResult<Readiness>;
  } = $props();

  let busyEpoch = $state('');
  let epochNotice = $state('');
  let epochError = $state('');

  async function acknowledgeEpoch(processEpoch: string, gateway: string) {
    if (
      !window.confirm(
        `Acknowledge the investigated unclean epoch for ${gateway}? Retained gap evidence will not be removed.`
      )
    )
      return;
    busyEpoch = processEpoch;
    epochError = '';
    epochNotice = '';
    try {
      await acknowledgeRequestMetadataGatewayEpoch(processEpoch);
      epochNotice = `Epoch ${processEpoch} acknowledged. Historical completeness evidence remains retained.`;
      await Promise.all([epochs.refetch(), readiness.refetch()]);
    } catch (error) {
      epochError = errorMessage(error, 'The epoch could not be acknowledged.');
    } finally {
      busyEpoch = '';
    }
  }
</script>

<section class="section" aria-labelledby="epochs-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Request metadata durability</p>
      <h2 id="epochs-title">Unresolved gateway epochs</h2>
      <p class="section-description">
        An unclean process epoch keeps readiness degraded until an operator
        investigates and acknowledges it. Acknowledgement is audited and never
        deletes its retained loss or uncertainty evidence.
      </p>
    </div>
    {#if epochs.data}<span
        class:warning={epochs.data.items.length > 0}
        class:success={epochs.data.items.length === 0}
        class="badge">{epochs.data.items.length} on page</span
      >{/if}
  </div>
  {#if epochNotice}<div class="inline-notice" role="status">
      {epochNotice}
    </div>{/if}
  {#if epochError}<div class="inline-problem" role="alert">
      {epochError}
    </div>{/if}
  {#if epochs.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(epochs.error, 'Gateway epochs are unavailable.')}
      <button class="text-button" onclick={() => epochs.refetch()}
        >Try again</button
      >
    </div>
  {:else if !epochs.data}
    <div class="loading-state" role="status">Loading gateway epochs…</div>
  {:else if epochs.data.items.length === 0 && pagination.history.length === 0}
    <div class="card empty-state">
      No unclean gateway epoch awaits acknowledgement.
    </div>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      class="table-shell"
      tabindex="0"
      role="region"
      aria-label="Unresolved request metadata gateway epochs"
    >
      <table class="data-table">
        <caption class="sr-only"
          >Unclean request metadata gateway process epochs awaiting operator
          acknowledgement</caption
        ><thead
          ><tr
            ><th scope="col">Gateway</th><th scope="col">Lifecycle</th><th
              scope="col">Writer</th
            ><th scope="col">Accepted / persisted</th><th scope="col"
              >Dropped / abandoned</th
            ><th scope="col">Uncertain lower bound</th><th scope="col"
              >Acknowledged</th
            ><th scope="col"><span class="sr-only">Action</span></th></tr
          ></thead
        ><tbody
          >{#each epochs.data.items as epoch (epoch.process_epoch)}<tr
              ><td
                ><strong>{epoch.gateway_instance}</strong><code
                  >{epoch.process_epoch}</code
                ></td
              ><td
                >Started {formatDate(epoch.started_at)}<small
                  >Detected {formatDate(
                    epoch.stale_detected_at ?? epoch.updated_at
                  )}</small
                ><small
                  >{epoch.gracefully_closed_at
                    ? `Closed ${formatDate(epoch.gracefully_closed_at)}`
                    : 'Never closed gracefully'}</small
                ></td
              ><td
                ><span class="badge {epoch.retrying ? 'warning' : ''}"
                  >{epoch.retrying ? 'Retrying' : 'Not retrying'}</span
                ><small
                  >{epoch.writer_closed
                    ? 'Writer closed'
                    : 'Writer open'}</small
                ></td
              ><td>{epoch.accepted} / {epoch.persisted}</td><td
                >{epoch.dropped} / {epoch.abandoned}</td
              ><td>{epoch.uncertain_event_lower_bound}</td><td
                >{epoch.acknowledged_at
                  ? formatDate(epoch.acknowledged_at)
                  : 'Not acknowledged'}{#if epoch.acknowledged_by}<small
                    class="mono">{epoch.acknowledged_by}</small
                  >{/if}</td
              ><td
                ><button
                  class="button button-secondary"
                  type="button"
                  onclick={() =>
                    acknowledgeEpoch(
                      epoch.process_epoch,
                      epoch.gateway_instance
                    )}
                  disabled={Boolean(busyEpoch)}
                  >{busyEpoch === epoch.process_epoch
                    ? 'Acknowledging…'
                    : 'Acknowledge epoch'}</button
                ></td
              ></tr
            >{/each}</tbody
        >
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(
        pagination,
        epochs.isPlaceholderData ? null : epochs.data.nextCursor
      )}
      label="Unresolved gateway epoch pages"
    />
  {/if}
</section>

<style>
  .section-description {
    max-width: 58rem;
    margin: 0.35rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
    overflow-wrap: anywhere;
  }
  td small,
  td code {
    display: block;
    margin-top: 0.15rem;
    color: var(--foreground-muted);
    font-size: 0.7rem;
    font-weight: 500;
  }
</style>
