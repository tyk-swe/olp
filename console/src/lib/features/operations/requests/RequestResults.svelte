<script lang="ts">
  import { resolve } from '$app/paths';

  import { errorMessage } from '$lib/api/http';
  import { cursorPaginationProps } from '$lib/lists/pagination';
  import {
    formatCost,
    formatDate,
    formatInteger,
    statusLabel,
    statusTone
  } from '$lib/format';
  import { type RequestListState } from './requestListState';

  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RequestSummary } from '$lib/api/requests';
  import type { CursorPage } from '$lib/api/http';
  let {
    requests,
    listState = $bindable(),
    applyFilters,
    resetFilters
  }: {
    requests: CreateQueryResult<CursorPage<RequestSummary>>;
    listState: RequestListState;
    applyFilters: (event: SubmitEvent) => void;
    resetFilters: () => void;
  } = $props();
</script>

<form class="card filters" aria-label="Request filters" onsubmit={applyFilters}>
  <div class="filter-grid">
    <label
      >Route <input
        bind:value={listState.route}
        name="route"
        placeholder="support-chat"
      /></label
    >
    <label
      >Operation <input
        bind:value={listState.operation}
        name="operation"
        placeholder="generation"
      /></label
    >
    <label
      >Provider ID <input
        bind:value={listState.providerId}
        name="provider"
        class="mono"
      /></label
    >
    <label>Model <input bind:value={listState.model} name="model" /></label>
    <label
      >API key ID <input
        bind:value={listState.apiKeyId}
        name="key"
        class="mono"
      /></label
    >
    <label
      >Status code <input
        bind:value={listState.statusCode}
        name="status"
        inputmode="numeric"
        pattern="[0-9][0-9][0-9]"
      /></label
    >
    <label
      >Error class <input
        bind:value={listState.errorClass}
        name="error"
      /></label
    >
    <label
      >Started after <input
        bind:value={listState.startedAfter}
        name="after"
        type="datetime-local"
      /></label
    >
    <label
      >Started before <input
        bind:value={listState.startedBefore}
        name="before"
        type="datetime-local"
      /></label
    >
  </div>
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply filters</button>
    <button class="button button-secondary" type="button" onclick={resetFilters}
      >Clear</button
    >
  </div>
</form>

<div class="toolbar">
  <p class="result-note" aria-live="polite">
    {requests.data?.items.length ?? 0} requests on this page
  </p>
  <button
    class="text-button"
    type="button"
    onclick={() => requests.refetch()}
    disabled={requests.isFetching}>Refresh</button
  >
</div>

{#if requests.isPending}
  <div class="loading-state" role="status">Loading request metadata…</div>
{:else if requests.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(requests.error, 'Request metadata is unavailable.')}
    <button class="text-button" onclick={() => requests.refetch()}
      >Try again</button
    >
  </div>
{:else if requests.data?.items.length === 0 && listState.history.length === 0}
  <div class="card empty-state">
    <div>
      <strong>No matching requests</strong>
      <p>Adjust the filters or send traffic through an active route.</p>
    </div>
  </div>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="table-shell desktop-results"
    tabindex="0"
    role="region"
    aria-label="Request results"
  >
    <table class="data-table">
      <caption class="sr-only">Request metadata, newest first</caption>
      <thead
        ><tr
          ><th scope="col">Started</th><th scope="col">Route / operation</th><th
            scope="col">Status</th
          ><th scope="col">Attempts</th><th scope="col">TTFT / latency</th><th
            scope="col">Tokens</th
          ><th scope="col">Cost</th><th scope="col"
            ><span class="sr-only">Details</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each requests.data?.items ?? [] as request (request.id)}
          <tr>
            <td
              >{formatDate(request.started_at)}<small
                >{request.completed_at
                  ? `Completed ${formatDate(request.completed_at)}`
                  : 'In flight'}</small
              ></td
            >
            <td
              ><strong>{request.route}</strong><small
                >{request.operation} · {request.surface}</small
              ></td
            >
            <td
              ><span
                class="badge {statusTone(
                  request.status_code,
                  request.error_class
                )}"
                >{statusLabel(request.status_code, request.error_class)}</span
              ></td
            >
            <td>{request.attempt_count}</td>
            <td
              >{request.first_byte_ms == null
                ? '—'
                : `${request.first_byte_ms} ms`} / {request.total_latency_ms ==
              null
                ? '—'
                : `${request.total_latency_ms} ms`}</td
            >
            <td
              >{formatInteger(request.input_tokens)} in<br />{formatInteger(
                request.output_tokens
              )} out<small
                >{formatInteger(request.cached_input_tokens)} cached</small
              ></td
            >
            <td
              ><span class:unpriced={request.unpriced}
                >{formatCost(request.estimated_cost, request.currency)}</span
              >{#if request.usage_complete === false}<small class="warning-text"
                  >Incomplete usage</small
                >{/if}</td
            >
            <td
              ><a
                class="row-link"
                href={resolve(`/requests/${request.id}`)}
                aria-label={`View request ${request.id}`}>View</a
              ></td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <ul class="mobile-results" aria-label="Request results">
    {#each requests.data?.items ?? [] as request (request.id)}
      <li class="card">
        <div class="mobile-result-heading">
          <div>
            <strong>{request.route}</strong><small
              >{request.operation} · {request.surface}</small
            >
          </div>
          <span
            class="badge {statusTone(request.status_code, request.error_class)}"
            >{statusLabel(request.status_code, request.error_class)}</span
          >
        </div>
        <dl>
          <div>
            <dt>Started</dt>
            <dd>{formatDate(request.started_at)}</dd>
          </div>
          <div>
            <dt>Completed</dt>
            <dd>
              {request.completed_at
                ? formatDate(request.completed_at)
                : 'In flight'}
            </dd>
          </div>
          <div>
            <dt>TTFT / latency</dt>
            <dd>
              {request.first_byte_ms == null
                ? '—'
                : `${request.first_byte_ms} ms`} / {request.total_latency_ms ==
              null
                ? '—'
                : `${request.total_latency_ms} ms`}
            </dd>
          </div>
          <div>
            <dt>Tokens</dt>
            <dd>
              {formatInteger(request.input_tokens)} in · {formatInteger(
                request.output_tokens
              )} out · {formatInteger(request.cached_input_tokens)} cached
            </dd>
          </div>
          <div>
            <dt>Cost</dt>
            <dd class:unpriced={request.unpriced}>
              {formatCost(request.estimated_cost, request.currency)}
            </dd>
          </div>
        </dl>
        <a
          class="button button-secondary"
          href={resolve(`/requests/${request.id}`)}
          aria-label={`View request ${request.id}`}>View timeline</a
        >
      </li>
    {/each}
  </ul>
  {@const pagination = cursorPaginationProps(
    listState,
    requests.isPlaceholderData ? null : requests.data?.nextCursor
  )}
  <nav class="pagination" aria-label="Request pages">
    <button
      class="button button-secondary"
      type="button"
      onclick={pagination.onPrevious}
      disabled={!pagination.hasPrevious}>Previous</button
    >
    <span>Page {pagination.page}</span>
    <button
      class="button button-secondary"
      type="button"
      onclick={pagination.onNext}
      disabled={!pagination.hasNext}>Next</button
    >
  </nav>
{/if}

<style>
  .filters {
    margin-top: 1.5rem;
    padding: 1rem;
  }
  .filter-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.8rem;
  }
  label {
    display: grid;
    gap: 0.35rem;
    color: var(--foreground-muted);
    font-size: 0.75rem;
    font-weight: 700;
  }
  input {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .filter-actions {
    display: flex;
    gap: 0.65rem;
    margin-top: 1rem;
  }
  .result-note {
    margin: 0;
    color: var(--foreground-muted);
  }
  .text-button {
    padding: 0.4rem 0.65rem;
  }
  .text-button:hover {
    text-decoration: underline;
  }
  td strong,
  td small {
    display: block;
  }
  td small {
    margin-top: 0.15rem;
    color: var(--foreground-muted);
  }
  .warning-text,
  .unpriced {
    color: var(--warning);
  }
  .row-link {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    color: var(--accent-strong);
    font-weight: 700;
  }
  .mobile-results {
    display: none;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .mobile-results li {
    padding: 1rem;
  }
  .mobile-results li > .button {
    width: 100%;
    margin-top: 0.85rem;
  }
  .mobile-result-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 0.75rem;
  }
  .mobile-result-heading strong,
  .mobile-result-heading small {
    display: block;
  }
  .mobile-result-heading small {
    margin-top: 0.15rem;
    color: var(--foreground-muted);
  }
  .pagination {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 1rem;
    margin-top: 1rem;
  }
  .pagination span {
    color: var(--foreground-muted);
  }
  .request-facts {
    margin-top: 1rem;
    padding: 1.25rem;
  }

  dl {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 1rem;
    margin: 1.25rem 0 0;
  }
  dl div {
    min-width: 0;
  }
  dt {
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  dd {
    overflow-wrap: anywhere;
    margin: 0.2rem 0 0;
  }
  .timeline-section {
    margin-top: 2rem;
  }
  .section-heading,
  .attempt-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .timeline {
    display: grid;
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }

  .timeline-marker {
    position: absolute;
    top: 0.85rem;
    left: -1.4rem;
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border-radius: 999px;
    background: var(--accent);
    color: white;
    font-size: 0.75rem;
    font-weight: 800;
  }

  @media (max-width: 72rem) {
    .filter-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 44rem) {
    .desktop-results {
      display: none;
    }
    .mobile-results {
      display: grid;
      gap: 0.75rem;
    }
  }
  @media (max-width: 40rem) {
    .filter-grid,
    dl {
      grid-template-columns: 1fr;
    }
    .mobile-results dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .filters {
      padding: 0.85rem;
    }
    .pagination {
      justify-content: space-between;
    }
  }
  @media (forced-colors: active) {
    .timeline-marker {
      border: 1px solid CanvasText;
    }
  }
</style>
