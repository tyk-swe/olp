<script lang="ts">
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import { formatDate, stateLabel } from '#lib/format.js';
  import { timeInputType } from '#lib/listing/filters.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import { cursorPaginationProps } from '#lib/listing/pagination.js';
  import { mediaJobStates, mediaJobLifecycles } from './mediaJobListState.js';
  import type { MediaJob } from './api.js';
  import type { MediaJobListState } from './mediaJobListState.js';

  let {
    listState = $bindable(),
    problem,
    blocked,
    pending,
    error,
    items,
    hasHistory,
    placeholder,
    nextCursor,
    onApply,
    onClear,
    onRetry
  }: {
    listState: MediaJobListState;
    problem: string | null;
    blocked: boolean;
    pending: boolean;
    error: string | null;
    items: MediaJob[];
    hasHistory: boolean;
    placeholder: boolean;
    nextCursor: string | null | undefined;
    onApply: (event: SubmitEvent) => void;
    onClear: () => void;
    onRetry: () => void;
  } = $props();

  function tone(state: string) {
    if (state === 'succeeded') return 'success';
    if (state === 'failed' || state === 'cancelled') return 'danger';
    return 'warning';
  }
</script>

<form class="card filters" aria-label="Media job filters" onsubmit={onApply}>
  <label>
    Route <input bind:value={listState.route} placeholder="All routes" />
  </label>

  <label>
    State

    <select bind:value={listState.jobState}>
      <option value="">All states</option>

      {#if listState.jobState && !mediaJobStates.includes(listState.jobState)}
        <option value={listState.jobState}>{listState.jobState}</option>
      {/if}{#each mediaJobStates as value (value)}
        <option {value}>{value}</option>
      {/each}
    </select>
  </label>

  <label>
    Lifecycle

    <select bind:value={listState.lifecycle}>
      <option value="">All lifecycles</option>

      {#if listState.lifecycle && !mediaJobLifecycles.includes(listState.lifecycle)}
        <option value={listState.lifecycle}>{listState.lifecycle}</option>
      {/if}{#each mediaJobLifecycles as value (value)}
        <option {value}>{stateLabel(value)}</option>
      {/each}
    </select>
  </label>

  <label>
    API key ID

    <input
      bind:value={listState.apiKeyId}
      class="mono"
      placeholder="All keys"
    />
  </label>
  <label>
    Provider ID <input
      bind:value={listState.providerId}
      class="mono"
      placeholder="All providers"
    />
  </label>
  <label>
    Created after <input
      bind:value={listState.createdAfter}
      type={timeInputType(listState.createdAfter)}
    />
  </label>
  <label>
    Created before <input
      bind:value={listState.createdBefore}
      type={timeInputType(listState.createdBefore)}
    />
  </label>
  <div class="filter-actions">
    <button class="button button-primary" type="submit">
      Apply filters
    </button><button
      class="button button-secondary"
      type="button"
      onclick={onClear}
    >
      Clear
    </button>
  </div>
</form>
{#if problem}<div class="inline-problem" role="alert">
    {problem}
  </div>{/if}
{#if !blocked}
  {#if pending}<div class="loading-state" role="status">
      Loading media jobs…
    </div>
  {:else if error}<div class="inline-problem" role="alert">
      {error}
      <button class="text-button" type="button" onclick={onRetry}>Retry</button>
    </div>
  {:else if items.length === 0 && !hasHistory}<section class="card empty-state">
      <p>No media jobs match these filters.</p>
    </section>
  {:else}<!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
    <div
      class="table-shell job-table-shell"
      tabindex="0"
      role="region"
      aria-label="Media job results"
    >
      <table class="data-table job-table">
        <caption class="sr-only">Asynchronous media jobs</caption><thead>
          <tr>
            <th scope="col">Route / operation</th><th scope="col">
              Provider
            </th><th scope="col">State</th><th scope="col">Lifecycle</th><th
              scope="col"
            >
              Progress
            </th><th scope="col">Created</th><th scope="col">Updated</th><th
              scope="col"
            >
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead><tbody>
          {#each items as job (job.id)}<tr>
              <td data-label="Route / operation">
                <strong>{job.route}</strong><small>{job.operation}</small>
              </td><td data-label="Provider">
                {job.provider_name}<small>{job.provider_model}</small><small
                  class="mono"
                >
                  {job.provider_id}
                </small>
              </td><td data-label="State">
                <span class={`badge ${tone(job.state)}`}>{job.state}</span>
              </td><td data-label="Lifecycle">
                {stateLabel(job.lifecycle)}
              </td><td data-label="Progress">
                {job.progress_percent == null
                  ? '—'
                  : `${job.progress_percent}%`}
              </td><td data-label="Created">{formatDate(job.created_at)}</td><td
                data-label="Updated"
              >
                {formatDate(job.updated_at)}
              </td><td class="job-action">
                <a
                  class="button button-secondary"
                  href={resolve(`media-jobs/${job.id}${page.url.search}`)}
                >
                  View
                </a>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(listState, placeholder ? null : nextCursor)}
      label="Media job pages"
    />{/if}
{/if}

<style>
  .filters {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
    margin: 1.5rem 0;
    padding: 1.25rem;
  }
  .filters label {
    display: grid;
    gap: 0.4rem;
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
  /* Operator-typed values are content, not labels: they keep the body weight
     instead of inheriting the label's emphasis. */
  .filters input,
  .filters select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-size: var(--text-body-sm);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .filters input:hover,
  .filters select:hover {
    border-color: var(--border-strong);
  }
  .filter-actions {
    display: flex;
    gap: 0.5rem;
  }
  td strong,
  td small {
    display: block;
  }
  td small {
    color: var(--foreground-muted);
  }
  /* Eight columns still cannot fit a phone. Left as a scrolling table, the row
     width also pushes mobile browsers into shrinking the whole page. */
  @media (max-width: 48rem) {
    .filters {
      display: grid;
    }
    .job-table-shell {
      overflow: visible;
      border: 0;
      background: transparent;
    }
    .job-table,
    .job-table tbody {
      display: grid;
      gap: 0.75rem;
    }
    .job-table thead {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
    }
    .job-table tbody tr {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
      gap: 0.75rem;
      padding: 1rem;
      border: 1px solid var(--border-hairline);
      border-radius: var(--radius-card);
      background: transparent;
    }
    .job-table tbody td {
      display: block;
      min-width: 0;
      min-height: 0;
      padding: 0;
      border: 0;
      overflow-wrap: anywhere;
    }
    .job-table tbody td::before {
      display: block;
      margin-bottom: 0.25rem;
      color: var(--foreground-subtle);
      content: attr(data-label);
      font-family: var(--font-mono);
      font-size: var(--text-caption);
      font-weight: 400;
      letter-spacing: -0.02em;
      text-transform: uppercase;
    }
    .job-table tbody td:first-child,
    .job-table tbody .job-action {
      grid-column: 1 / -1;
    }
    .job-table tbody .job-action::before {
      display: none;
    }
    .job-table tbody .job-action .button {
      width: 100%;
    }
  }
</style>
