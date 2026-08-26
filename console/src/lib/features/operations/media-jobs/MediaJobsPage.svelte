<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    getMediaJob,
    listMediaJobs
  } from '$lib/api/media-jobs';
  import { errorMessage } from '$lib/api/http';
  import { cursorPaginationProps, resetCursor } from '$lib/api/pagination';
  import { formatDate } from '$lib/format';
  import {
    emptyMediaJobListState,
    mediaJobFilters,
    type MediaJobListState
  } from './mediaJobListState';

  let {
    jobId = '',
    listState = $bindable()
  }: {
    jobId?: string;
    listState: MediaJobListState;
  } = $props();

  const jobs = createQuery(() => ({
    queryKey: ['media-jobs', listState.applied, listState.cursor ?? 'first'],
    queryFn: () => listMediaJobs({ ...listState.applied, cursor: listState.cursor }),
    placeholderData: (previous) => previous,
    enabled: !jobId
  }));
  const detail = createQuery(() => ({
    queryKey: ['media-job', jobId],
    queryFn: () => getMediaJob(jobId),
    enabled: Boolean(jobId)
  }));

  function apply(event: SubmitEvent) {
    event.preventDefault();
    resetCursor(listState);
    listState.applied = mediaJobFilters(listState);
  }

  function clear() {
    Object.assign(listState, emptyMediaJobListState());
  }

  function tone(value: string) {
    if (['succeeded', 'deleted'].includes(value)) return 'success';
    if (['failed', 'cancelled', 'create_ambiguous'].includes(value)) return 'danger';
    return 'warning';
  }
</script>

<svelte:head><title>Media Jobs · OpenLLMProxy</title></svelte:head>

{#if jobId}
  <div class="page-header"><div><p class="eyebrow">Operations · Media job</p><h1 class="page-title">Media job detail</h1><p class="page-description">Lifecycle and reconciliation metadata only. Uploaded and generated media never appears in the console.</p></div><a class="button button-secondary" href={resolve('/media-jobs')}>All media jobs</a></div>
  {#if detail.isPending}<div class="loading-state" role="status">Loading media job…</div>
  {:else if detail.isError}<div class="inline-problem" role="alert">{errorMessage(detail.error, 'The media job could not be loaded.')} <button class="text-button" type="button" onclick={() => detail.refetch()}>Retry</button></div>
  {:else if detail.data}
    <section class="card job-detail" aria-labelledby="job-state-heading">
      <div class="section-heading"><div><p class="eyebrow">{detail.data.operation}</p><h2 id="job-state-heading">{detail.data.route}</h2></div><span class={`badge ${tone(detail.data.state)}`}>{detail.data.state}</span></div>
      <dl><div><dt>Lifecycle</dt><dd>{detail.data.lifecycle.replaceAll('_', ' ')}</dd></div><div><dt>Progress</dt><dd>{detail.data.progress_percent == null ? '—' : `${detail.data.progress_percent}%`}</dd></div><div><dt>Provider</dt><dd>{detail.data.provider_name}<small>{detail.data.provider_model}</small></dd></div><div><dt>Client surface</dt><dd>{detail.data.surface}</dd></div><div><dt>Content status</dt><dd>{detail.data.content_available ? 'Available through the authenticated vendor API' : 'Not available'}</dd></div><div><dt>Created</dt><dd>{formatDate(detail.data.created_at)}</dd></div><div><dt>Completed</dt><dd>{detail.data.completed_at ? formatDate(detail.data.completed_at) : 'Not finished'}</dd></div><div><dt>Last polled</dt><dd>{detail.data.last_polled_at ? formatDate(detail.data.last_polled_at) : 'Never polled'}</dd></div><div><dt>Expires</dt><dd>{detail.data.expires_at ? formatDate(detail.data.expires_at) : 'No retention deadline'}</dd></div><div><dt>Deleted</dt><dd>{detail.data.deleted_at ? formatDate(detail.data.deleted_at) : 'Not deleted'}</dd></div><div><dt>Updated</dt><dd>{formatDate(detail.data.updated_at)}</dd></div></dl>
      {#if detail.data.error_class || detail.data.reconciliation_error}<div class="inline-problem" role="alert"><strong>{detail.data.error_class ?? 'Reconciliation error'}</strong><p>{detail.data.reconciliation_error ?? 'The upstream job failed.'}</p></div>{/if}
      <div class="identifiers"><p><strong>OLP job</strong><code>{detail.data.id}</code></p><p><strong>Upstream job</strong><code>{detail.data.upstream_job_id ?? 'not assigned'}</code></p><p><strong>API key ID</strong><code>{detail.data.api_key_id}</code></p><p><strong>Provider ID</strong><code>{detail.data.provider_id}</code></p></div>
    </section>
  {/if}
{:else}
  <div class="page-header"><div><p class="eyebrow">Operations</p><h1 class="page-title">Media Jobs</h1><p class="page-description">Track asynchronous video and media lifecycles without exposing uploaded or generated content.</p></div><button class="button button-secondary" type="button" onclick={() => jobs.refetch()} disabled={jobs.isFetching}>Refresh</button></div>
  <form class="card filters" aria-label="Media job filters" onsubmit={apply}>
    <label>Route <input bind:value={listState.route} placeholder="All routes" /></label>
    <label>State <select bind:value={listState.jobState}><option value="">All states</option>{#each ['queued', 'running', 'succeeded', 'failed', 'cancelled'] as value (value)}<option value={value}>{value}</option>{/each}</select></label>
    <label>Lifecycle <select bind:value={listState.lifecycle}><option value="">All lifecycles</option>{#each ['creating', 'active', 'create_ambiguous', 'create_cleanup_pending', 'delete_pending', 'deleted'] as value (value)}<option value={value}>{value.replaceAll('_', ' ')}</option>{/each}</select></label>
    <label>API key ID <input bind:value={listState.apiKeyId} class="mono" placeholder="All keys" /></label>
    <label>Provider ID <input bind:value={listState.providerId} class="mono" placeholder="All providers" /></label>
    <label>Created after <input bind:value={listState.createdAfter} type="datetime-local" /></label>
    <label>Created before <input bind:value={listState.createdBefore} type="datetime-local" /></label>
    <div class="filter-actions"><button class="button button-primary" type="submit">Apply filters</button><button class="button button-secondary" type="button" onclick={clear}>Clear</button></div>
  </form>
  {#if jobs.isPending}<div class="loading-state" role="status">Loading media jobs…</div>
  {:else if jobs.isError}<div class="inline-problem" role="alert">{errorMessage(jobs.error, 'Media jobs are unavailable.')} <button class="text-button" type="button" onclick={() => jobs.refetch()}>Retry</button></div>
  {:else if jobs.data?.items.length === 0 && listState.history.length === 0}<section class="card empty-state"><p>No media jobs match these filters.</p></section>
  {:else}<!-- svelte-ignore a11y_no_noninteractive_tabindex --><div class="table-shell job-table-shell" tabindex="0" role="region" aria-label="Media job results"><table class="data-table job-table"><caption class="sr-only">Asynchronous media jobs</caption><thead><tr><th scope="col">Route / operation</th><th scope="col">Provider</th><th scope="col">State</th><th scope="col">Lifecycle</th><th scope="col">Progress</th><th scope="col">Created</th><th scope="col">Updated</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each jobs.data?.items ?? [] as job (job.id)}<tr><td data-label="Route / operation"><strong>{job.route}</strong><small>{job.operation}</small></td><td data-label="Provider">{job.provider_name}<small>{job.provider_model}</small><small class="mono">{job.provider_id}</small></td><td data-label="State"><span class={`badge ${tone(job.state)}`}>{job.state}</span></td><td data-label="Lifecycle">{job.lifecycle.replaceAll('_', ' ')}</td><td data-label="Progress">{job.progress_percent == null ? '—' : `${job.progress_percent}%`}</td><td data-label="Created">{formatDate(job.created_at)}</td><td data-label="Updated">{formatDate(job.updated_at)}</td><td class="job-action"><a class="button button-secondary" href={resolve(`/media-jobs/${job.id}`)}>View</a></td></tr>{/each}</tbody></table></div><CursorPagination {...cursorPaginationProps(listState, jobs.isPlaceholderData ? null : jobs.data?.nextCursor)} label="Media job pages" />{/if}
{/if}

<style>
  .filters { display: flex; flex-wrap: wrap; align-items: end; gap: .65rem; margin: 1.25rem 0; padding: 1rem; }
  .filters label { display: grid; gap: .3rem; color: var(--foreground-muted); font-size: .72rem; font-weight: 700; }
  /* Operator-typed values are content, not labels: they keep the body size and
     weight instead of inheriting the label's small bold. */
  .filters input, .filters select { min-height: 2.5rem; padding: .5rem .7rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); font-size: .875rem; font-weight: 400; }
  .filter-actions { display: flex; gap: .5rem; }
  td strong, td small, dd small { display: block; }
  td small, dd small { color: var(--foreground-muted); }
  .job-detail { max-width: 64rem; margin-top: 1.5rem; padding: 1.25rem; }
  .section-heading { display: flex; align-items: start; justify-content: space-between; gap: 1rem; }
  h2 { margin: 0; }
  dl { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .75rem; }
  dl div, .identifiers p { min-width: 0; padding: .75rem; border-radius: .375rem; background: var(--surface-subtle); }
  dt { color: var(--foreground-muted); font-size: .7rem; font-weight: 700; }
  dd { margin: .15rem 0 0; font-weight: 700; overflow-wrap: anywhere; }
  .identifiers { display: grid; gap: .5rem; margin-top: 1rem; }
  .identifiers p { display: grid; gap: .25rem; margin: 0; }
  .identifiers code { overflow-wrap: anywhere; font: .72rem 'JetBrains Mono Variable', monospace; }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  /* Eight columns still cannot fit a phone. Left as a scrolling table, the row
     width also pushes mobile browsers into shrinking the whole page. */
  @media (max-width: 48rem) {
    dl { grid-template-columns: 1fr; }
    .filters { display: grid; }
    .job-table-shell { overflow: visible; border: 0; background: transparent; box-shadow: none; }
    .job-table, .job-table tbody { display: grid; gap: .75rem; }
    .job-table thead { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; }
    .job-table tbody tr { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: .75rem; padding: 1rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface); box-shadow: var(--shadow-sm); }
    .job-table tbody td { display: block; min-width: 0; min-height: 0; padding: 0; border: 0; overflow-wrap: anywhere; }
    .job-table tbody td::before { display: block; margin-bottom: .2rem; color: var(--foreground-muted); content: attr(data-label); font-size: .68rem; font-weight: 760; letter-spacing: .045em; text-transform: uppercase; }
    .job-table tbody td:first-child, .job-table tbody .job-action { grid-column: 1 / -1; }
    .job-table tbody .job-action::before { display: none; }
    .job-table tbody .job-action .button { width: 100%; }
  }
</style>
