<script lang="ts">
  import NavIcon from '#lib/components/NavIcon.svelte';
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
    <div class="loading-state" role="status">
      Loading recent request metadata…
    </div>
  {:else if services.error}
    <div class="inline-problem" role="alert">
      Request history capabilities are unavailable.
      <button
        class="text-button"
        type="button"
        onclick={() => services.retry()}
      >
        Try again
      </button>
    </div>
  {:else if !services.retentionEnforced}
    <div class="empty-state" role="status">
      <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
      <strong>Request history is not retained yet</strong>
      <p>
        This installation does not store request metadata, so recent requests
        cannot be listed here.
      </p>
    </div>
  {:else if recentRequests.isPending}
    <div class="loading-state" role="status">
      Loading recent request metadata…
    </div>
  {:else if recentRequests.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(
        recentRequests.error,
        'Recent request metadata is unavailable.'
      )}
      <button
        class="text-button"
        type="button"
        onclick={() => recentRequests.refetch()}
      >
        Try again
      </button>
    </div>
  {:else if !recentRequests.data?.items.length}
    <div class="empty-state">
      <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
      <strong>No request metadata yet</strong>
      <p>
        Successful and failed attempts will appear here after a route is active.
      </p>
    </div>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
    <div
      class="table-shell recent-table"
      tabindex="0"
      role="region"
      aria-label="Five most recent requests"
    >
      <table class="data-table">
        <thead>
          <tr>
            <th>Started</th><th>Route</th><th>Operation</th><th>Status</th><th>
              Latency
            </th><th><span class="sr-only">Details</span></th>
          </tr>
        </thead>
        <tbody>
          {#each recentRequests.data.items as request (request.id)}
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

              <td>
                {request.total_latency_ms == null
                  ? '—'
                  : `${request.total_latency_ms} ms`}
              </td>

              <td>
                <a class="row-link" href={resolve(`requests/${request.id}`)}>
                  View timeline
                </a>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .recent-table {
    margin-top: 1rem;
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
    flex: none;
    margin-top: 0;
  }

  .empty-state > span {
    display: grid;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-muted);
  }

  .empty-state strong {
    color: var(--foreground);
  }

  .empty-state p {
    max-width: 34rem;
    margin: 0;
    font-size: var(--text-body-sm);
  }
</style>
