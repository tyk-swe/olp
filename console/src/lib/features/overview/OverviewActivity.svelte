<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate, statusLabel, statusTone } from '#lib/format.js';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { CursorPage } from '#lib/management/http.js';
  import type { RequestSummary } from '#lib/features/usage/index.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';

  const {
    services,
    recentRequests
  }: {
    services: ReturnType<typeof useServiceCapabilities>;
    recentRequests: CreateQueryResult<CursorPage<RequestSummary>>;
  } = $props();
</script>

<section class="card activity" aria-labelledby="activity-title">
  <div class="activity-heading">
    <div>
      <p class="eyebrow">Operations</p>
      <h2 id="activity-title">Recent requests</h2>
    </div>

    <a href={resolve('requests')}>
      Explore requests
      <NavIcon name="arrow" size={17} />
    </a>
  </div>
  {#if services.pending}
    <div class="loading-state compact" role="status">
      Loading recent request metadata…
    </div>
  {:else if services.error}
    <div class="activity-state">
      <Callout tone="danger">
        Request history capabilities are unavailable.
        {#snippet actions()}<button
            class="button button-secondary"
            type="button"
            onclick={() => services.retry()}
          >
            Try again
          </button>{/snippet}
      </Callout>
    </div>
  {:else if !services.retentionEnforced}
    <div class="activity-state">
      <EmptyState title="Request history is not retained yet">
        This installation does not store request metadata, so recent requests
        cannot be listed here.
      </EmptyState>
    </div>
  {:else}
    <div class="activity-state">
      <QueryView
        query={recentRequests}
        compact
        loading="Loading recent request metadata…"
        failure={errorMessage(
          recentRequests.error,
          'Recent request metadata is unavailable.'
        )}
        retryLabel="Try again"
        emptyWhen={!recentRequests.data?.items.length}
      >
        {#snippet empty()}
          <EmptyState title="No request metadata yet">
            Successful and failed attempts will appear here after a route is
            active.
          </EmptyState>
        {/snippet}
        <ScrollTable label="Five most recent requests">
          <table class="data-table recent-table">
            <thead>
              <tr>
                <th>Started</th><th>Route</th><th>Operation</th><th>
                  Status
                </th><th>Latency</th><th>
                  <span class="sr-only">Details</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {#each recentRequests.data?.items ?? [] as request (request.id)}
                <tr>
                  <td>{formatDate(request.started_at)}</td>
                  <td><code>{request.route}</code></td>
                  <td>{request.operation} · {request.surface}</td>

                  <td>
                    <span
                      class="badge {statusTone(
                        request.status_code,
                        request.error_class
                      )}"
                    >
                      {statusLabel(request.status_code, request.error_class)}
                    </span>
                  </td>

                  <td class="num">
                    {request.total_latency_ms == null
                      ? '—'
                      : `${request.total_latency_ms} ms`}
                  </td>

                  <td>
                    <a
                      class="table-link"
                      href={resolve(`requests/${request.id}`)}
                    >
                      View timeline
                    </a>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </ScrollTable>
      </QueryView>
    </div>
  {/if}
</section>

<style>
  .activity-state {
    margin-top: 1rem;
  }

  .recent-table {
    min-width: 42rem;
  }

  .activity {
    margin-top: 1rem;
    padding: 1.5rem;
  }

  .activity-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }

  .activity-heading > a {
    display: inline-flex;
    min-height: 2.5rem;
    flex: none;
    align-items: center;
    gap: 0.4rem;
    margin-top: 0;
    color: var(--foreground);
    font-size: var(--text-body-sm);
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition:
      color var(--motion),
      text-decoration-color var(--motion);
  }

  .activity-heading > a:hover {
    color: var(--foreground-hover);
    text-decoration-color: var(--signal);
  }
</style>
