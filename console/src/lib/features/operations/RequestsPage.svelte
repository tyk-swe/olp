<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import {
    getRequest,
    listRequests,
    type RequestFilters
  } from '$lib/api/operations';
  import { formatCompact, formatCost, formatDate, statusLabel, statusTone } from './format';

  let { path = 'requests' }: { path?: string } = $props();
  const requestId = $derived(path.split('/')[1]);

  let route = $state('');
  let providerId = $state('');
  let model = $state('');
  let apiKeyId = $state('');
  let operation = $state('');
  let statusCode = $state('');
  let errorClass = $state('');
  let startedAfter = $state('');
  let startedBefore = $state('');
  let applied = $state<RequestFilters>({ limit: 25 });
  let cursors = $state<string[]>([]);

  const requests = createQuery(() => ({
    queryKey: ['requests', JSON.stringify(applied)],
    queryFn: () => listRequests(applied),
    enabled: !requestId
  }));

  const detail = createQuery(() => ({
    queryKey: ['request', requestId],
    queryFn: () => getRequest(requestId),
    enabled: Boolean(requestId)
  }));

  function iso(value: string) {
    if (!value) return undefined;
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? undefined : date.toISOString();
  }

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    cursors = [];
    applied = {
      limit: 25,
      route: route || undefined,
      provider_id: providerId || undefined,
      model: model || undefined,
      api_key_id: apiKeyId || undefined,
      operation: operation || undefined,
      status_code: statusCode ? Number(statusCode) : undefined,
      error_class: errorClass || undefined,
      started_after: iso(startedAfter),
      started_before: iso(startedBefore)
    };
  }

  function resetFilters() {
    route = providerId = model = apiKeyId = operation = statusCode = errorClass = startedAfter = startedBefore = '';
    cursors = [];
    applied = { limit: 25 };
  }

  function nextPage() {
    const cursor = requests.data?.next_cursor;
    if (!cursor) return;
    cursors = [...cursors, cursor];
    applied = { ...applied, cursor };
  }

  function previousPage() {
    const history = [...cursors];
    history.pop();
    const cursor = history.at(-1);
    cursors = history;
    applied = { ...applied, cursor };
  }
</script>

<svelte:head><title>Requests · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">{requestId ? 'Request timeline' : 'Request Explorer'}</h1>
    <p class="page-description">
      {requestId
        ? 'Metadata-only route decisions and upstream attempts. Request and response content is never available here.'
        : 'Filter operational metadata by route, target, key, outcome, or time range—never prompt or output content.'}
    </p>
  </div>
  {#if requestId}<a class="button button-secondary" href="/requests">Back to requests</a>{/if}
</div>

{#if requestId}
  {#if detail.isPending}
    <div class="loading-state" role="status">Loading request timeline…</div>
  {:else if detail.isError}
    <div class="inline-problem" role="alert">The request timeline could not be loaded. <button class="text-button" onclick={() => detail.refetch()}>Try again</button></div>
  {:else if detail.data}
    <section class="metric-grid" aria-label="Request summary">
      <article class="card metric-card"><p>Status</p><strong><span class="badge {statusTone(detail.data.status_code, detail.data.error_class)}">{statusLabel(detail.data.status_code, detail.data.error_class)}</span></strong></article>
      <article class="card metric-card"><p>Total latency</p><strong>{detail.data.total_latency_ms ?? '—'} ms</strong></article>
      <article class="card metric-card"><p>First byte (TTFT)</p><strong>{detail.data.first_byte_ms ?? '—'} ms</strong></article>
      <article class="card metric-card"><p>Estimated cost</p><strong class:unpriced={detail.data.unpriced}>{formatCost(detail.data.estimated_cost, detail.data.currency ?? 'USD')}</strong></article>
    </section>

    <section class="card request-facts" aria-labelledby="decision-title">
      <div>
        <p class="eyebrow">Route decision</p>
        <h2 id="decision-title">{detail.data.route}</h2>
      </div>
      <dl>
        <div><dt>Operation</dt><dd>{detail.data.operation}</dd></div>
        <div><dt>Client surface</dt><dd>{detail.data.surface}</dd></div>
        <div><dt>Runtime generation</dt><dd class="mono">{detail.data.runtime_generation_id}</dd></div>
        <div><dt>API key lookup</dt><dd class="mono">{detail.data.api_key_id}</dd></div>
        <div><dt>Input tokens</dt><dd>{formatCompact(detail.data.input_tokens)}</dd></div>
        <div><dt>Output tokens</dt><dd>{formatCompact(detail.data.output_tokens)}</dd></div>
        <div><dt>Usage completeness</dt><dd><span class="badge" class:success={detail.data.usage_complete} class:warning={!detail.data.usage_complete}>{detail.data.usage_complete ? 'Complete' : 'Incomplete'}</span></dd></div>
        <div><dt>Started</dt><dd>{formatDate(detail.data.started_at)}</dd></div>
      </dl>
    </section>

    <section class="timeline-section" aria-labelledby="attempts-title">
      <div class="section-heading">
        <div><p class="eyebrow">Upstream</p><h2 id="attempts-title">Attempt timeline</h2></div>
        <span class="badge">{detail.data.attempts.length} attempts</span>
      </div>
      {#if detail.data.attempts.length === 0}
        <div class="card empty-state">No attempt metadata was recorded.</div>
      {:else}
        <ol class="timeline">
          {#each detail.data.attempts as attempt (attempt.id)}
            <li class="card">
              <span class="timeline-marker" aria-hidden="true">{attempt.ordinal}</span>
              <div class="attempt-heading">
                <div><strong>{attempt.provider_name}</strong><span class="mono">{attempt.upstream_model}</span></div>
                <span class="badge {statusTone(attempt.status_code, attempt.error_class)}">{statusLabel(attempt.status_code, attempt.error_class)}</span>
              </div>
              <dl>
                <div><dt>Started</dt><dd>{formatDate(attempt.started_at)}</dd></div>
                <div><dt>First byte</dt><dd>{attempt.first_byte_ms === null || attempt.first_byte_ms === undefined ? '—' : `${attempt.first_byte_ms} ms`}</dd></div>
                <div><dt>Latency</dt><dd>{attempt.latency_ms === null || attempt.latency_ms === undefined ? '—' : `${attempt.latency_ms} ms`}</dd></div>
                <div><dt>Response committed</dt><dd>{attempt.committed ? 'Yes — failover stopped' : 'No'}</dd></div>
              </dl>
            </li>
          {/each}
        </ol>
      {/if}
    </section>
  {/if}
{:else}
  <form class="card filters" aria-label="Request filters" onsubmit={applyFilters}>
    <div class="filter-grid">
      <label>Route <input bind:value={route} name="route" placeholder="support-chat" /></label>
      <label>Operation <input bind:value={operation} name="operation" placeholder="generation" /></label>
      <label>Provider ID <input bind:value={providerId} name="provider" class="mono" /></label>
      <label>Model <input bind:value={model} name="model" /></label>
      <label>API key ID <input bind:value={apiKeyId} name="key" class="mono" /></label>
      <label>Status code <input bind:value={statusCode} name="status" inputmode="numeric" pattern="[0-9]{3}" /></label>
      <label>Error class <input bind:value={errorClass} name="error" /></label>
      <label>Started after <input bind:value={startedAfter} name="after" type="datetime-local" /></label>
      <label>Started before <input bind:value={startedBefore} name="before" type="datetime-local" /></label>
    </div>
    <div class="filter-actions">
      <button class="button button-primary" type="submit">Apply filters</button>
      <button class="button button-secondary" type="button" onclick={resetFilters}>Clear</button>
    </div>
  </form>

  <div class="toolbar">
    <p class="result-note" aria-live="polite">{requests.data?.data.length ?? 0} requests on this page</p>
    <button class="text-button" type="button" onclick={() => requests.refetch()} disabled={requests.isFetching}>Refresh</button>
  </div>

  {#if requests.isPending}
    <div class="loading-state" role="status">Loading request metadata…</div>
  {:else if requests.isError}
    <div class="inline-problem" role="alert">Request metadata is unavailable. <button class="text-button" onclick={() => requests.refetch()}>Try again</button></div>
  {:else if requests.data?.data.length === 0}
    <div class="card empty-state"><div><strong>No matching requests</strong><p>Adjust the filters or send traffic through an active route.</p></div></div>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div class="table-shell desktop-results" tabindex="0" role="region" aria-label="Request results">
      <table class="data-table">
        <caption class="sr-only">Request metadata, newest first</caption>
        <thead><tr><th scope="col">Started</th><th scope="col">Route / operation</th><th scope="col">Status</th><th scope="col">Attempts</th><th scope="col">TTFT / latency</th><th scope="col">Tokens</th><th scope="col">Cost</th><th scope="col"><span class="sr-only">Details</span></th></tr></thead>
        <tbody>
          {#each requests.data?.data ?? [] as request (request.id)}
            <tr>
              <td>{formatDate(request.started_at)}</td>
              <td><strong>{request.route}</strong><small>{request.operation} · {request.surface}</small></td>
              <td><span class="badge {statusTone(request.status_code, request.error_class)}">{statusLabel(request.status_code, request.error_class)}</span></td>
              <td>{request.attempt_count}</td>
              <td>{request.first_byte_ms ?? '—'} / {request.total_latency_ms ?? '—'} ms</td>
              <td>{formatCompact(request.input_tokens)} in<br />{formatCompact(request.output_tokens)} out</td>
              <td><span class:unpriced={request.unpriced}>{formatCost(request.estimated_cost, request.currency ?? 'USD')}</span>{#if request.usage_complete === false}<small class="warning-text">Incomplete usage</small>{/if}</td>
              <td><a class="row-link" href={`/requests/${request.id}`} aria-label={`View request ${request.id}`}>View</a></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <ul class="mobile-results" aria-label="Request results">
      {#each requests.data?.data ?? [] as request (request.id)}
        <li class="card">
          <div class="mobile-result-heading"><div><strong>{request.route}</strong><small>{request.operation} · {request.surface}</small></div><span class="badge {statusTone(request.status_code, request.error_class)}">{statusLabel(request.status_code, request.error_class)}</span></div>
          <dl><div><dt>Started</dt><dd>{formatDate(request.started_at)}</dd></div><div><dt>TTFT / latency</dt><dd>{request.first_byte_ms ?? '—'} / {request.total_latency_ms ?? '—'} ms</dd></div><div><dt>Tokens</dt><dd>{formatCompact(request.input_tokens)} in · {formatCompact(request.output_tokens)} out</dd></div><div><dt>Cost</dt><dd class:unpriced={request.unpriced}>{formatCost(request.estimated_cost, request.currency ?? 'USD')}</dd></div></dl>
          <a class="button button-secondary" href={`/requests/${request.id}`} aria-label={`View request ${request.id}`}>View timeline</a>
        </li>
      {/each}
    </ul>
    <nav class="pagination" aria-label="Request pages">
      <button class="button button-secondary" type="button" onclick={previousPage} disabled={cursors.length === 0}>Previous</button>
      <span>Page {cursors.length + 1}</span>
      <button class="button button-secondary" type="button" onclick={nextPage} disabled={!requests.data?.next_cursor}>Next</button>
    </nav>
  {/if}
{/if}

<style>
  .filters { margin-top: 1.5rem; padding: 1rem; }
  .filter-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 0.8rem; }
  label { display: grid; gap: 0.35rem; color: var(--foreground-muted); font-size: 0.75rem; font-weight: 700; }
  input { width: 100%; min-height: 2.5rem; padding: 0.5rem 0.7rem; border: 1px solid var(--border-strong); border-radius: 0.375rem; background: var(--surface); color: var(--foreground); }
  .filter-actions { display: flex; gap: 0.65rem; margin-top: 1rem; }
  .result-note { margin: 0; color: var(--foreground-muted); }
  .text-button { min-height: 2.75rem; padding: 0.4rem 0.65rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  .text-button:hover { text-decoration: underline; }
  td strong, td small { display: block; }
  td small { margin-top: 0.15rem; color: var(--foreground-muted); }
  .warning-text, .unpriced { color: var(--warning); }
  .row-link { display: inline-flex; min-height: 2.75rem; align-items: center; color: var(--accent-strong); font-weight: 700; }
  .mobile-results { display: none; margin: 0; padding: 0; list-style: none; }
  .mobile-results li { padding: 1rem; }
  .mobile-results li > .button { width: 100%; margin-top: 0.85rem; }
  .mobile-result-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 0.75rem; }
  .mobile-result-heading strong, .mobile-result-heading small { display: block; }
  .mobile-result-heading small { margin-top: 0.15rem; color: var(--foreground-muted); }
  .pagination { display: flex; align-items: center; justify-content: flex-end; gap: 1rem; margin-top: 1rem; }
  .pagination span { color: var(--foreground-muted); }
  .request-facts { margin-top: 1rem; padding: 1.25rem; }
  h2 { margin: 0; font-size: 1.2rem; letter-spacing: -0.025em; }
  dl { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 1rem; margin: 1.25rem 0 0; }
  dl div { min-width: 0; }
  dt { color: var(--foreground-muted); font-size: 0.72rem; font-weight: 700; }
  dd { overflow-wrap: anywhere; margin: 0.2rem 0 0; }
  .timeline-section { margin-top: 2rem; }
  .section-heading, .attempt-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; }
  .timeline { display: grid; gap: 0.75rem; margin: 1rem 0 0; padding: 0; list-style: none; }
  .timeline li { position: relative; margin-left: 1.4rem; padding: 1rem 1rem 1rem 1.5rem; }
  .timeline-marker { position: absolute; top: 0.85rem; left: -1.4rem; display: grid; width: 2rem; height: 2rem; place-items: center; border-radius: 999px; background: var(--accent); color: white; font-size: 0.75rem; font-weight: 800; }
  .attempt-heading strong, .attempt-heading .mono { display: block; }
  .attempt-heading .mono { margin-top: 0.15rem; color: var(--foreground-muted); font-size: 0.78rem; }
  .timeline dl { grid-template-columns: repeat(4, minmax(0, 1fr)); }
  @media (max-width: 72rem) { .filter-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } dl, .timeline dl { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
  @media (max-width: 44rem) { .desktop-results { display: none; } .mobile-results { display: grid; gap: 0.75rem; } }
  @media (max-width: 40rem) { .filter-grid, dl, .timeline dl { grid-template-columns: 1fr; } .mobile-results dl { grid-template-columns: repeat(2, minmax(0, 1fr)); } .filters { padding: 0.85rem; } .pagination { justify-content: space-between; } }
  @media (forced-colors: active) { .timeline-marker { border: 1px solid CanvasText; } }
</style>
