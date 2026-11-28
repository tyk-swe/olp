<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import SessionTimeline from './SessionTimeline.svelte';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { errorMessage } from '#lib/management/http.js';
  import { cursorPaginationProps } from '#lib/listing/pagination.js';
  import {
    formatCost,
    formatDate,
    formatInteger,
    statusLabel,
    statusTone
  } from '#lib/format.js';
  import type { RequestListState } from '#lib/features/usage/history/requestListState.js';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { RequestSummary } from '#lib/domain/requests/api.js';
  import type { CursorPage } from '#lib/management/http.js';

  let {
    requests,
    listState,
    queryProblem,
    exporting,
    exportError,
    onExport
  }: {
    requests: CreateQueryResult<CursorPage<RequestSummary>>;
    listState: RequestListState;
    queryProblem: string | null;
    exporting: boolean;
    exportError: string;
    onExport: () => void;
  } = $props();
</script>

{#if !queryProblem && listState.applied.session_id && requests.data && !requests.isPending && !requests.isError && !requests.isPlaceholderData}
  <SessionTimeline
    items={requests.data.items}
    sessionId={listState.applied.session_id}
  />
{/if}

{#if !queryProblem}
  <div class="toolbar">
    <p class="result-note" aria-live="polite">
      {requests.data?.items.length ?? 0} requests on this page
    </p>
    <button
      class="text-button"
      type="button"
      onclick={onExport}
      disabled={exporting}
    >
      {exporting ? 'Exporting…' : 'Export CSV'}
    </button><button
      class="text-button"
      type="button"
      onclick={() => requests.refetch()}
      disabled={requests.isFetching}
    >
      Refresh
    </button>
  </div>
  {#if exportError}<div class="export-problem">
      <Callout tone="danger">{exportError}</Callout>
    </div>{/if}

  <QueryView
    query={requests}
    loading="Loading request metadata…"
    failure={errorMessage(requests.error, 'Request metadata is unavailable.')}
    retryLabel="Try again"
    updating={requests.isPlaceholderData}
    emptyWhen={requests.data?.items.length === 0 &&
      listState.history.length === 0}
  >
    {#snippet empty()}
      <EmptyState title="No matching requests">
        Adjust the filters or send traffic through an active route.
      </EmptyState>
    {/snippet}
    <div class="desktop-results">
      <ScrollTable label="Request results" busy={requests.isPlaceholderData}>
        <table class="data-table">
          <caption class="sr-only">Request metadata, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Started</th><th scope="col">
                Route / operation
              </th><th scope="col">Status</th><th scope="col" class="num">
                Attempts
              </th><th scope="col" class="num">TTFT / latency</th><th
                scope="col"
                class="num"
              >
                Tokens
              </th><th scope="col" class="num">Cost</th><th scope="col">
                <span class="sr-only">Details</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {#each requests.data?.items ?? [] as request (request.id)}
              <tr>
                <td>
                  {formatDate(request.started_at)}<small>
                    {request.completed_at
                      ? `Completed ${formatDate(request.completed_at)}`
                      : 'In flight'}
                  </small>
                </td>
                <td>
                  <strong>{request.route}</strong><small>
                    {request.operation} · {request.surface}
                  </small>
                </td>
                <td>
                  <span
                    class="badge {statusTone(
                      request.status_code,
                      request.error_class
                    )}"
                  >
                    {statusLabel(request.status_code, request.error_class)}
                  </span>{#if request.payload_captured}<br /><small
                      class="capture-note"
                      title="Queued in the volatile capture queue — does not guarantee delivery"
                    >
                      Capture queued
                    </small>{/if}
                </td>
                <td class="num">{request.attempt_count}</td>
                <td class="num">
                  {request.first_byte_ms == null
                    ? '—'
                    : `${request.first_byte_ms} ms`} / {request.total_latency_ms ==
                  null
                    ? '—'
                    : `${request.total_latency_ms} ms`}
                </td>
                <td class="num">
                  {formatInteger(request.input_tokens)} in<br />{formatInteger(
                    request.output_tokens
                  )} out<small>
                    {formatInteger(request.cached_input_tokens)} cached · {formatInteger(
                      request.cache_write_input_tokens
                    )} write
                  </small>
                </td>
                <td class="num">
                  <span class:unpriced={request.unpriced}>
                    {formatCost(request.estimated_cost, request.currency)}
                  </span>{#if request.usage_complete === false}<small
                      class="warning-text"
                    >
                      Incomplete usage
                    </small>{/if}
                </td>
                <td>
                  <a
                    class="table-link"
                    href={resolve(`requests/${request.id}${page.url.search}`)}
                    aria-label={`View request ${request.id}`}
                  >
                    View
                  </a>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    </div>
    <ul class="mobile-results" aria-label="Request results">
      {#each requests.data?.items ?? [] as request (request.id)}
        <li class="card">
          <div class="mobile-result-heading">
            <div>
              <strong>{request.route}</strong><small>
                {request.operation} · {request.surface}
              </small>
            </div>
            <span
              class="badge {statusTone(
                request.status_code,
                request.error_class
              )}"
            >
              {statusLabel(request.status_code, request.error_class)}
            </span>
            {#if request.payload_captured}<small
                class="capture-note"
                title="Queued in the volatile capture queue — does not guarantee delivery"
              >
                Capture queued
              </small>{/if}
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
                )} out · {formatInteger(request.cached_input_tokens)} cached · {formatInteger(
                  request.cache_write_input_tokens
                )} cache write
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
            href={resolve(`requests/${request.id}${page.url.search}`)}
            aria-label={`View request ${request.id}`}
          >
            View timeline
          </a>
        </li>
      {/each}
    </ul>
    <CursorPagination
      {...cursorPaginationProps(
        listState,
        requests.isPlaceholderData ? null : requests.data?.nextCursor
      )}
      label="Request pages"
    />
  </QueryView>
{/if}

<style>
  .export-problem {
    margin-bottom: 0.75rem;
  }
  .capture-note {
    color: var(--warning);
    font-size: var(--text-caption);
  }
  .result-note {
    margin: 0;
    color: var(--foreground-muted);
  }
  .text-button {
    padding: 0.4rem 0.65rem;
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
  .mobile-results {
    display: none;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .mobile-results li {
    padding: 1.25rem;
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
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  dd {
    overflow-wrap: anywhere;
    margin: 0.25rem 0 0;
  }

  @media (max-width: 72rem) {
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
    dl {
      grid-template-columns: 1fr;
    }
    .mobile-results dl {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
</style>
